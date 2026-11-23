package atestore

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

var _ orchestration.TaskReassignmentRepository = (*Store)(nil)

// taskSessionFence is an optional capability, not a sequence re-read. A session
// provider implementing it MUST prove, using only this transaction's records,
// that the loaded session prefix is complete and cannot advance before commit.
// Every session append must participate in that same authority-root CAS (or
// there must be an irrevocable seal). It must never acquire another native
// client, load session history, or call authority.View/Update here.
//
// The current session.EventStore API alone cannot provide this guarantee.
// Without this capability ReassignTask fails closed after checking the proof;
// terminal Agent state and two equal CurrentSequence reads are not a fence.
type taskSessionFence interface {
	ValidateTaskSessionFence(tx *ateauthority.Tx, id session.ID, sequence uint64) error
}

// nativeTaskSessions identifies an authoritative native Session collection.
// Its sequence is checked at the transaction anchor and protected by ATE's
// publication fence, including when sessions live in another Space.
type nativeTaskSessions interface {
	SessionAuthority() (model.StoreID, model.SpaceID)
}

type taskRetryEvidence struct {
	Agent    orchestration.AgentSnapshot
	Sequence uint64
}

func (store *Store) ReassignTask(ctx context.Context, id orchestration.TaskID, failedAgent orchestration.AgentID, expected uint64, reason string) (orchestration.TaskSnapshot, error) {
	ctx = taskContext(ctx)
	if expected == 0 || expected == orchestration.AnySequence {
		return orchestration.TaskSnapshot{}, fmt.Errorf("%w: exact task version is required", orchestration.ErrTaskStateConflict)
	}
	var before orchestration.TaskSnapshot
	var agents []orchestration.AgentSnapshot
	replayed := false
	// Release the native process lease before loading authoritative sessions.
	err := store.authority.View(ctx, func(tx *ateauthority.Tx) error {
		var err error
		before, agents, replayed, err = taskRetryCheck(tx, id, failedAgent, expected)
		return err
	})
	if err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	if replayed {
		return before, nil
	}
	if store.sessions == nil {
		return orchestration.TaskSnapshot{}, fmt.Errorf("%w: authoritative session history is unavailable", orchestration.ErrTaskReassignUnsafe)
	}
	proofs := make([]taskRetryEvidence, 0, len(agents))
	for _, agent := range agents {
		if agent.SessionID == "" {
			return orchestration.TaskSnapshot{}, fmt.Errorf("%w: attempt has no session identity", orchestration.ErrTaskReassignUnsafe)
		}
		sequence, err := store.sessions.CurrentSequence(ctx, agent.SessionID)
		if err != nil {
			return orchestration.TaskSnapshot{}, err
		}
		// Stream evidence into a bounded proof accumulator, never a history cache.
		proof := taskNewReadOnlyProof(agent.CurrentRunID, agent.ID == failedAgent)
		var after uint64
		for after < sequence {
			events, err := store.sessions.Load(ctx, agent.SessionID, after, 256)
			if err != nil {
				return orchestration.TaskSnapshot{}, err
			}
			if len(events) == 0 {
				return orchestration.TaskSnapshot{}, fmt.Errorf("%w: truncated session history", orchestration.ErrTaskReassignUnsafe)
			}
			for _, event := range events {
				if event.SessionID != agent.SessionID || event.Sequence != after+1 || event.Sequence > sequence {
					return orchestration.TaskSnapshot{}, fmt.Errorf("%w: session history changed or is discontinuous", orchestration.ErrTaskReassignUnsafe)
				}
				if err := proof.consume(event); err != nil {
					return orchestration.TaskSnapshot{}, err
				}
				after = event.Sequence
			}
		}
		if err := proof.finish(); err != nil {
			return orchestration.TaskSnapshot{}, err
		}
		proofs = append(proofs, taskRetryEvidence{Agent: agent, Sequence: sequence})
	}
	var result orchestration.TaskSnapshot
	err = store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		task, currentAgents, replay, err := taskRetryCheck(tx, id, failedAgent, expected)
		if err != nil {
			return err
		}
		if replay {
			result = task
			return nil
		}
		if taskHash(before) != taskHash(task) || taskHash(agents) != taskHash(currentAgents) {
			return fmt.Errorf("%w: attempt changed while loading history", orchestration.ErrTaskReassignUnsafe)
		}
		native, nativeOK := store.sessions.(nativeTaskSessions)
		fence, fencedOK := store.sessions.(taskSessionFence)
		if !nativeOK && !fencedOK {
			return fmt.Errorf("%w: session provider cannot fence authoritative history with the task transaction; reassignment is disabled", orchestration.ErrTaskReassignUnsafe)
		}
		for _, evidence := range proofs {
			var err error
			if nativeOK {
				storeID, space := native.SessionAuthority()
				err = tx.RequireSessionSequence(storeID, space, string(evidence.Agent.SessionID), evidence.Sequence)
			} else {
				err = fence.ValidateTaskSessionFence(tx, evidence.Agent.SessionID, evidence.Sequence)
			}
			if err != nil {
				return fmt.Errorf("%w: session fence: %w", orchestration.ErrTaskReassignUnsafe, err)
			}
		}
		agent, err := getRecord[orchestration.AgentSnapshot](tx, "agent", string(failedAgent))
		if err != nil {
			return err
		}
		previous := task.Attempt
		task.State, task.AssigneeAgentID, task.ClaimExpiresAt = orchestration.TaskReady, "", nil
		task.Attempt++
		task.Version++
		task.UpdatedAt = time.Now().UTC()
		if err := putRecord(tx, "task", string(id), task); err != nil {
			return err
		}
		if err := taskEvent(tx, task, orchestration.PendingEvent{Type: orchestration.EventTaskReassigned, ActorAgentID: failedAgent, CorrelationID: string(agent.CurrentRunID), Payload: orchestration.MustJSONPayload(map[string]any{"from": orchestration.TaskFailed, "to": orchestration.TaskReady, "failed_agent_id": failedAgent, "failed_run_id": agent.CurrentRunID, "previous_attempt": previous, "attempt": task.Attempt, "retry_code": agent.Result.RetryCode, "reason": reason})}); err != nil {
			return err
		}
		if err := createRecord(tx, "task-reassign-receipt", taskPair(string(id), string(failedAgent)), taskReceipt{TaskID: id, RunID: agent.CurrentRunID}); err != nil {
			return err
		}
		result = task
		return nil
	})
	if err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	return result, nil
}

// taskRetryCheck proves assignment identity and quiescence in the same snapshot
// as the eventual write. Repeating it protects subtree and workspace phantoms.
func taskRetryCheck(tx *ateauthority.Tx, id orchestration.TaskID, failedAgent orchestration.AgentID, expected uint64) (orchestration.TaskSnapshot, []orchestration.AgentSnapshot, bool, error) {
	task, err := getRecord[orchestration.TaskSnapshot](tx, "task", string(id))
	if err != nil {
		return task, nil, false, err
	}
	agent, err := getRecord[orchestration.AgentSnapshot](tx, "agent", string(failedAgent))
	if err != nil {
		return task, nil, false, err
	}
	if agent.TeamID != task.TeamID || agent.AssignedTaskID != task.ID || agent.CurrentRunID == "" {
		return task, nil, false, fmt.Errorf("%w: failed assignment identity does not match", orchestration.ErrTaskReassignUnsafe)
	}
	var receipt taskReceipt
	found, err := tx.Get(recordKey("task-reassign-receipt", taskPair(string(id), string(failedAgent))), &receipt)
	if err != nil {
		return task, nil, false, err
	}
	if found {
		if receipt.TaskID != id || receipt.RunID != agent.CurrentRunID {
			return task, nil, false, orchestration.ErrIdempotencyConflict
		}
		return task, nil, true, nil
	}
	if err := taskActiveTeam(tx, task.TeamID); err != nil {
		return task, nil, false, err
	}
	if task.Version != expected {
		return task, nil, false, orchestration.ErrTaskStateConflict
	}
	if task.State != orchestration.TaskFailed || task.AssigneeAgentID != agent.ID {
		return task, nil, false, orchestration.ErrTaskClaimConflict
	}
	if task.Attempt >= orchestration.MaxTaskAttempts-1 {
		return task, nil, false, orchestration.ErrTaskAttemptLimit
	}
	if agent.State != orchestration.AgentFailed || agent.Result == nil || agent.Result.Status != orchestration.ResultFailed || !agent.Result.Retryable || !agent.Result.RetryCode.Retryable() || agent.Result.OutcomeUnknown || agent.Result.Delivery != nil {
		return task, nil, false, fmt.Errorf("%w: failure has no trusted retry classification", orchestration.ErrTaskReassignUnsafe)
	}
	events, err := listRecords[orchestration.Event](tx, "event")
	if err != nil {
		return task, nil, false, err
	}
	var latest orchestration.Event
	for _, event := range events {
		if event.StreamType == orchestration.StreamTask && event.StreamID == string(id) && event.Type == orchestration.EventTaskUpdated && event.StreamSequence > latest.StreamSequence {
			latest = event
		}
	}
	var assignment struct {
		To       orchestration.TaskState `json:"to"`
		Assignee orchestration.AgentID   `json:"assignee_agent_id"`
		Attempt  int                     `json:"attempt"`
	}
	if err := json.Unmarshal(latest.Payload, &assignment); err != nil {
		return task, nil, false, fmt.Errorf("%w: missing or corrupt task failure receipt: %v", orchestration.ErrTaskReassignUnsafe, err)
	}
	if latest.CorrelationID != string(agent.CurrentRunID) || assignment.To != orchestration.TaskFailed || assignment.Assignee != agent.ID || assignment.Attempt != task.Attempt {
		return task, nil, false, fmt.Errorf("%w: failed task receipt belongs to another attempt/run", orchestration.ErrTaskReassignUnsafe)
	}
	all, err := listRecords[orchestration.AgentSnapshot](tx, "agent")
	if err != nil {
		return task, nil, false, err
	}
	members := map[orchestration.AgentID]bool{agent.ID: true}
	for changed := true; changed; {
		changed = false
		for _, child := range all {
			if members[child.ParentAgentID] && !members[child.ID] {
				members[child.ID] = true
				changed = true
			}
		}
	}
	subtree := make([]orchestration.AgentSnapshot, 0, len(members))
	for _, member := range all {
		if !members[member.ID] {
			continue
		}
		if !taskAgentTerminal(member.State) {
			return task, nil, false, fmt.Errorf("%w: attempt has live descendants", orchestration.ErrTaskReassignUnsafe)
		}
		subtree = append(subtree, member)
	}
	slices.SortFunc(subtree, func(a, b orchestration.AgentSnapshot) int { return strings.Compare(string(a.ID), string(b.ID)) })
	for _, kind := range []string{"lease", "workspace-view", "merge"} {
		records, err := listRecords[taskWorkspaceRecord](tx, kind)
		if err != nil {
			return task, nil, false, err
		}
		for _, record := range records {
			inSubtree := members[record.AgentID]
			unsafe := false
			switch kind {
			case "lease":
				unsafe = inSubtree && record.State != "released" && record.State != "abandoned" && record.State != "rejected"
			case "workspace-view":
				unsafe = inSubtree && record.State != "released"
			case "merge":
				unsafe = inSubtree || record.TaskID == id
			}
			if unsafe {
				return task, nil, false, fmt.Errorf("%w: attempt has unproven %s quiescence or delivery history", orchestration.ErrTaskReassignUnsafe, kind)
			}
		}
	}
	return task, subtree, false, nil
}

type taskRetryTool struct {
	TurnID    string          `json:"turn_id"`
	CallID    string          `json:"call_id"`
	Tool      string          `json:"tool"`
	Revision  string          `json:"revision"`
	RiskClass string          `json:"risk_class"`
	Result    json.RawMessage `json:"result"`
}

type taskReadOnlyProof struct {
	run                           orchestration.RunID
	requireFailure, failureFound  bool
	identityBytes                 int
	turnRuns                      map[string]orchestration.RunID
	terminal                      map[string]bool
	proposals, started, succeeded map[string]taskRetryTool
}

func taskNewReadOnlyProof(run orchestration.RunID, requireFailure bool) *taskReadOnlyProof {
	return &taskReadOnlyProof{run: run, requireFailure: requireFailure, turnRuns: map[string]orchestration.RunID{}, terminal: map[string]bool{}, proposals: map[string]taskRetryTool{}, started: map[string]taskRetryTool{}, succeeded: map[string]taskRetryTool{}}
}

func (proof *taskReadOnlyProof) consume(event session.Event) error {
	unsafe := func(reason string) error { return fmt.Errorf("%w: %s", orchestration.ErrTaskReassignUnsafe, reason) }
	// Bound proof metadata as well as each session page. No partial proof is
	// accepted when this bound is reached.
	if len(proof.turnRuns)+len(proof.terminal)+len(proof.proposals)+len(proof.started)+len(proof.succeeded) > 65536 {
		return unsafe("session execution proof exceeds bound")
	}
	switch event.Type {
	case session.EventTurnStarted:
		var payload struct {
			TurnID string              `json:"turn_id"`
			RunID  orchestration.RunID `json:"run_id"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return unsafe("invalid turn identity JSON")
		}
		if payload.TurnID == "" {
			payload.TurnID = event.CorrelationID
		}
		if payload.TurnID == "" || payload.RunID == "" || payload.TurnID != event.CorrelationID {
			return unsafe("missing turn/run identity")
		}
		proof.identityBytes += len(payload.TurnID) + len(payload.RunID)
		proof.turnRuns[payload.TurnID] = payload.RunID
	case session.EventTurnCompleted, session.EventTurnFailed, session.EventTurnAbandoned:
		proof.identityBytes += len(event.CorrelationID)
		proof.terminal[event.CorrelationID] = true
		if event.Type == session.EventTurnFailed && proof.turnRuns[event.CorrelationID] == proof.run {
			proof.failureFound = true
		}
	case session.EventToolOutcomeUnknown:
		return unsafe("unknown tool outcome")
	case session.EventToolProposed, session.EventToolStarted, session.EventToolSucceeded:
		var call taskRetryTool
		if err := json.Unmarshal(event.Payload, &call); err != nil {
			return unsafe("invalid tool identity JSON")
		}
		if call.CallID == "" {
			call.CallID = event.CorrelationID
		}
		if call.TurnID == "" || call.CallID == "" || call.CallID != event.CorrelationID || call.Tool == "" || call.Revision == "" || proof.turnRuns[call.TurnID] == "" {
			return unsafe("incomplete tool identity")
		}
		key := call.TurnID + "\x00" + call.CallID
		proof.identityBytes += len(key) + len(call.TurnID) + len(call.CallID) + len(call.Tool) + len(call.Revision) + len(call.RiskClass)
		validResult := json.Valid(call.Result)
		call.Result = nil // Retain identity only, never tool output/history.
		switch event.Type {
		case session.EventToolProposed:
			proof.proposals[key] = call
		case session.EventToolStarted:
			proof.started[key] = call
		case session.EventToolSucceeded:
			if _, ok := proof.started[key]; !ok || !validResult {
				return unsafe("invalid tool success receipt")
			}
			proof.succeeded[key] = call
		}
	}
	if proof.identityBytes > 16<<20 {
		return unsafe("session execution proof identity bytes exceed bound")
	}
	return nil
}

func (proof *taskReadOnlyProof) finish() error {
	if proof.requireFailure && !proof.failureFound {
		return fmt.Errorf("%w: original run has no durable failed turn", orchestration.ErrTaskReassignUnsafe)
	}
	for turn := range proof.turnRuns {
		if !proof.terminal[turn] {
			return fmt.Errorf("%w: turn has no terminal receipt", orchestration.ErrTaskReassignUnsafe)
		}
	}
	for key, call := range proof.started {
		proposal, proposed := proof.proposals[key]
		success, completed := proof.succeeded[key]
		if !proposed || !completed || proposal.Tool != call.Tool || proposal.Revision != call.Revision || success.Tool != call.Tool || success.Revision != call.Revision || proposal.RiskClass != "pure" && proposal.RiskClass != "read" {
			return fmt.Errorf("%w: started tool lacks matching read-only success proof", orchestration.ErrTaskReassignUnsafe)
		}
	}
	return nil
}

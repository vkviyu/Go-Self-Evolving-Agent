package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

// ReassignTask preserves the task identity and failed Agent history. Only the
// claim and attempt change. The proof and CAS share one SQLite transaction;
// an idempotency receipt prevents a stale retry from consuming another attempt.
func (store *OrchestrationStore) ReassignTask(ctx context.Context, taskID orchestration.TaskID, failedAgentID orchestration.AgentID, expected uint64, reason string) (orchestration.TaskSnapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if expected == 0 || expected == orchestration.AnySequence {
		return orchestration.TaskSnapshot{}, fmt.Errorf("%w: exact task version is required", orchestration.ErrTaskStateConflict)
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return orchestration.TaskSnapshot{}, fmt.Errorf("begin task reassignment: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	task, err := taskByID(ctx, tx, taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return orchestration.TaskSnapshot{}, fmt.Errorf("%w: task %s", orchestration.ErrNotFound, taskID)
	}
	if err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	agent, err := agentByID(ctx, tx, failedAgentID)
	if err != nil {
		return orchestration.TaskSnapshot{}, fmt.Errorf("inspect failed Agent: %w", err)
	}
	if agent.TeamID != task.TeamID || agent.AssignedTaskID != task.ID || agent.CurrentRunID == "" {
		return orchestration.TaskSnapshot{}, fmt.Errorf("%w: failed assignment identity does not match", orchestration.ErrTaskReassignUnsafe)
	}
	scope := "task-reassign:" + string(taskID)
	var previousRun, previousTask string
	err = tx.QueryRowContext(ctx, `SELECT result_type, result_id FROM idempotency_records WHERE scope = ? AND idempotency_key = ?`, scope, string(failedAgentID)).Scan(&previousRun, &previousTask)
	if err == nil {
		if previousRun != string(agent.CurrentRunID) || previousTask != string(taskID) {
			return orchestration.TaskSnapshot{}, orchestration.ErrIdempotencyConflict
		}
		return task, nil // read-only replay, even if a later attempt has advanced
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return orchestration.TaskSnapshot{}, err
	}
	if err := requireActiveTeam(ctx, tx, task.TeamID); err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	if expected != task.Version {
		return orchestration.TaskSnapshot{}, fmt.Errorf("%w: expected %d, current %d", orchestration.ErrTaskStateConflict, expected, task.Version)
	}
	if task.State != orchestration.TaskFailed || task.AssigneeAgentID != agent.ID {
		return orchestration.TaskSnapshot{}, fmt.Errorf("%w: task is not in the failed assignment", orchestration.ErrTaskClaimConflict)
	}
	if task.Attempt >= orchestration.MaxTaskAttempts-1 {
		return orchestration.TaskSnapshot{}, orchestration.ErrTaskAttemptLimit
	}
	if agent.State != orchestration.AgentFailed || agent.Result == nil || agent.Result.Status != orchestration.ResultFailed || !agent.Result.Retryable || !agent.Result.RetryCode.Retryable() || agent.Result.OutcomeUnknown || agent.Result.Delivery != nil {
		return orchestration.TaskSnapshot{}, fmt.Errorf("%w: failure has no trusted retry classification", orchestration.ErrTaskReassignUnsafe)
	}
	if err := proveTaskAttemptRepeatable(ctx, tx, task, agent); err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := execCAS(ctx, tx, orchestration.ErrTaskStateConflict, `UPDATE tasks SET state = ?, assignee_agent_id = '', claim_expires_at = '', attempt = attempt + 1, version = version + 1, updated_at = ? WHERE task_id = ? AND version = ? AND state = ? AND assignee_agent_id = ?`, string(orchestration.TaskReady), now, string(taskID), task.Version, string(orchestration.TaskFailed), string(failedAgentID)); err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	seq, err := currentOrchestrationSequence(ctx, tx, orchestration.StreamTask, string(taskID))
	if err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	if _, err := appendOrchestrationEvents(ctx, tx, orchestration.Stream{Type: orchestration.StreamTask, ID: string(taskID), TeamID: task.TeamID}, seq, orchestration.PendingEvent{Type: orchestration.EventTaskReassigned, ActorAgentID: failedAgentID, CorrelationID: string(agent.CurrentRunID), Payload: orchestration.MustJSONPayload(map[string]any{"from": orchestration.TaskFailed, "to": orchestration.TaskReady, "failed_agent_id": failedAgentID, "failed_run_id": agent.CurrentRunID, "previous_attempt": task.Attempt, "attempt": task.Attempt + 1, "retry_code": agent.Result.RetryCode, "reason": reason})}); err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO idempotency_records(scope,idempotency_key,result_type,result_id,created_at) VALUES(?,?,?,?,?)`, scope, string(agent.ID), string(agent.CurrentRunID), string(task.ID), now); err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	updated, err := taskByID(ctx, tx, taskID)
	if err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return orchestration.TaskSnapshot{}, fmt.Errorf("commit task reassignment: %w", err)
	}
	return updated, nil
}

const retrySubtree = `WITH RECURSIVE subtree(agent_id) AS (SELECT ? UNION ALL SELECT child.agent_id FROM agents child JOIN subtree parent ON child.parent_agent_id = parent.agent_id) `

// A terminal projection alone does not establish quiescence. Leases, views,
// children, deliveries and tool receipts are independent evidence. The first
// implementation intentionally accepts only read-only attempts; successful
// writes are not replayed just because their results are known.
func proveTaskAttemptRepeatable(ctx context.Context, tx *sql.Tx, task orchestration.TaskSnapshot, agent orchestration.AgentSnapshot) error {
	failed, err := scanOrchestrationEvent(tx.QueryRowContext(ctx, `SELECT global_sequence,event_id,stream_type,stream_id,stream_sequence,team_id,event_type,occurred_at,actor_agent_id,causation_id,correlation_id,idempotency_key,payload,checksum,schema_version FROM orchestration_events WHERE stream_type = ? AND stream_id = ? AND event_type = ? ORDER BY stream_sequence DESC LIMIT 1`, string(orchestration.StreamTask), string(task.ID), string(orchestration.EventTaskUpdated)))
	if err != nil {
		return fmt.Errorf("%w: task failure receipt: %v", orchestration.ErrTaskReassignUnsafe, err)
	}
	var assignment struct {
		To       orchestration.TaskState `json:"to"`
		Assignee orchestration.AgentID   `json:"assignee_agent_id"`
		Attempt  int                     `json:"attempt"`
	}
	if err := json.Unmarshal(failed.Payload, &assignment); err != nil {
		return err
	}
	if failed.CorrelationID != string(agent.CurrentRunID) || assignment.To != orchestration.TaskFailed || assignment.Assignee != agent.ID || assignment.Attempt != task.Attempt {
		return fmt.Errorf("%w: failed task receipt belongs to another attempt/run", orchestration.ErrTaskReassignUnsafe)
	}
	checks := []struct {
		query, reason string
		args          []any
	}{
		{`SELECT COUNT(*) FROM agents WHERE agent_id IN (SELECT agent_id FROM subtree) AND state NOT IN ('completed','failed','canceled')`, "attempt has live descendants", []any{string(agent.ID)}},
		{`SELECT COUNT(*) FROM workspace_leases WHERE agent_id IN (SELECT agent_id FROM subtree) AND state NOT IN ('released','abandoned','rejected')`, "attempt has unproven lease quiescence", []any{string(agent.ID)}},
		{`SELECT COUNT(*) FROM workspace_views WHERE agent_id IN (SELECT agent_id FROM subtree) AND state <> 'released'`, "attempt has active workspace views", []any{string(agent.ID)}},
		{`SELECT COUNT(*) FROM merge_queue WHERE agent_id IN (SELECT agent_id FROM subtree) OR task_id = ?`, "attempt has workspace delivery history", []any{string(agent.ID), string(task.ID)}},
	}
	for _, check := range checks {
		var count int
		if err := tx.QueryRowContext(ctx, retrySubtree+check.query, check.args...).Scan(&count); err != nil {
			return fmt.Errorf("inspect reassignment safety: %w", err)
		}
		if count != 0 {
			return fmt.Errorf("%w: %s", orchestration.ErrTaskReassignUnsafe, check.reason)
		}
	}
	rows, err := tx.QueryContext(ctx, retrySubtree+`SELECT a.agent_id, th.session_id, a.current_run_id FROM agents a JOIN agent_threads th ON th.agent_id = a.agent_id WHERE a.agent_id IN (SELECT agent_id FROM subtree)`, string(agent.ID))
	if err != nil {
		return err
	}
	type identity struct {
		agentID   orchestration.AgentID
		sessionID session.ID
		runID     orchestration.RunID
	}
	var identities []identity
	for rows.Next() {
		var value identity
		if err := rows.Scan(&value.agentID, &value.sessionID, &value.runID); err != nil {
			_ = rows.Close()
			return err
		}
		identities = append(identities, value)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, value := range identities {
		if err := proveReadOnlySession(ctx, tx, value.sessionID, value.runID, value.agentID == agent.ID); err != nil {
			return err
		}
	}
	return nil
}

type retryToolIdentity struct {
	TurnID    string          `json:"turn_id"`
	CallID    string          `json:"call_id"`
	Tool      string          `json:"tool"`
	Revision  string          `json:"revision"`
	RiskClass string          `json:"risk_class"`
	Result    json.RawMessage `json:"result"`
}

func proveReadOnlySession(ctx context.Context, tx *sql.Tx, sessionID session.ID, failedRun orchestration.RunID, requireFailure bool) error {
	rows, err := tx.QueryContext(ctx, `SELECT `+sessionEventColumns+` FROM session_events WHERE session_id = ? ORDER BY sequence`, string(sessionID))
	if err != nil {
		return err
	}
	defer rows.Close()
	turnRuns := map[string]orchestration.RunID{}
	terminalTurns := map[string]bool{}
	proposals, started, succeeded := map[string]retryToolIdentity{}, map[string]retryToolIdentity{}, map[string]retryToolIdentity{}
	failureFound := false
	for rows.Next() {
		event, err := scanSessionEvent(rows, sessionID)
		if err != nil {
			return err
		}
		switch event.Type {
		case session.EventTurnStarted:
			var payload struct {
				TurnID string              `json:"turn_id"`
				RunID  orchestration.RunID `json:"run_id"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return err
			}
			if payload.TurnID == "" {
				payload.TurnID = event.CorrelationID
			}
			if payload.TurnID == "" || payload.RunID == "" || payload.TurnID != event.CorrelationID {
				return fmt.Errorf("%w: missing turn/run identity", orchestration.ErrTaskReassignUnsafe)
			}
			turnRuns[payload.TurnID] = payload.RunID
		case session.EventTurnCompleted, session.EventTurnFailed, session.EventTurnAbandoned:
			terminalTurns[event.CorrelationID] = true
			if event.Type == session.EventTurnFailed && turnRuns[event.CorrelationID] == failedRun {
				failureFound = true
			}
		case session.EventToolOutcomeUnknown:
			return fmt.Errorf("%w: unknown tool outcome", orchestration.ErrTaskReassignUnsafe)
		case session.EventToolProposed, session.EventToolStarted, session.EventToolSucceeded:
			var call retryToolIdentity
			if err := json.Unmarshal(event.Payload, &call); err != nil {
				return err
			}
			if call.CallID == "" {
				call.CallID = event.CorrelationID
			}
			if call.TurnID == "" || call.CallID == "" || call.CallID != event.CorrelationID || call.Tool == "" || call.Revision == "" || turnRuns[call.TurnID] == "" {
				return fmt.Errorf("%w: incomplete tool identity", orchestration.ErrTaskReassignUnsafe)
			}
			key := call.TurnID + "\x00" + call.CallID
			switch event.Type {
			case session.EventToolProposed:
				proposals[key] = call
			case session.EventToolStarted:
				started[key] = call
			case session.EventToolSucceeded:
				if _, ok := started[key]; !ok || !json.Valid(call.Result) {
					return fmt.Errorf("%w: invalid tool success receipt", orchestration.ErrTaskReassignUnsafe)
				}
				succeeded[key] = call
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if requireFailure && !failureFound {
		return fmt.Errorf("%w: original run has no durable failed turn", orchestration.ErrTaskReassignUnsafe)
	}
	for turn := range turnRuns {
		if !terminalTurns[turn] {
			return fmt.Errorf("%w: turn has no terminal receipt", orchestration.ErrTaskReassignUnsafe)
		}
	}
	for key, call := range started {
		proposal, proposed := proposals[key]
		success, completed := succeeded[key]
		if !proposed || !completed || proposal.Tool != call.Tool || proposal.Revision != call.Revision || success.Tool != call.Tool || success.Revision != call.Revision || (proposal.RiskClass != "pure" && proposal.RiskClass != "read") {
			return fmt.Errorf("%w: started tool lacks matching read-only success proof", orchestration.ErrTaskReassignUnsafe)
		}
	}
	return nil
}

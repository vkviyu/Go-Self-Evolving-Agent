package atestore

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentflow"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/retention"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

type retentionTurn struct {
	started, completed, closed bool
	snapshot                   string
}

// RetentionPlan reads lifecycle evidence exclusively from ATE. Local references
// add candidates, never terminal-state proof. The caller owns the shared
// StoreID+Session execution lock throughout preview or apply.
func (store *Store) RetentionPlan(ctx context.Context, id session.ID, local []retention.Consumer) (retention.Plan, error) {
	turns, sequence, err := store.retentionTurns(ctx, id)
	if err != nil {
		return retention.Plan{}, err
	}
	var plan retention.Plan
	err = store.authority.View(ctx, func(tx *ateauthority.Tx) (err error) {
		plan, err = planNativeRetention(tx, id, sequence, turns, local)
		return err
	})
	return plan, err
}

func (store *Store) retentionTurns(ctx context.Context, id session.ID) (map[string]retentionTurn, uint64, error) {
	if id == "" || len(id) > 4096 || store.sessions == nil {
		return nil, 0, errors.New("retention requires an explicit authoritative session")
	}
	turns := make(map[string]retentionTurn)
	var sequence uint64
	err := session.VisitEvents(ctx, store.sessions, id, func(e session.Event) error {
		sequence = e.Sequence
		switch e.Type {
		case session.EventTurnStarted, session.EventTurnCompleted, session.EventTurnArchived, session.EventTurnAbandoned:
		default:
			return nil
		}
		if e.CorrelationID == "" {
			return errors.New("turn lifecycle event lacks identity")
		}
		state := turns[e.CorrelationID]
		switch e.Type {
		case session.EventTurnStarted:
			if state.started || state.completed || state.closed {
				return errors.New("inconsistent retention turn start")
			}
			state.started, state.snapshot = true, e.RuntimeSnapshotID
		case session.EventTurnCompleted:
			if !state.started || state.completed || state.closed || (state.snapshot != "" && e.RuntimeSnapshotID != state.snapshot) {
				return errors.New("inconsistent retention turn completion")
			}
			state.completed = true
		default:
			state.closed = true
		}
		turns[e.CorrelationID] = state
		if len(turns) > 10000 {
			return errors.New("retention exceeds 10000 turns")
		}
		return nil
	})
	return turns, sequence, err
}

func planNativeRetention(tx *ateauthority.Tx, id session.ID, sequence uint64, source map[string]retentionTurn, local []retention.Consumer) (retention.Plan, error) {
	plan := retention.Plan{SessionID: id, Sequence: sequence, Consumers: []retention.Consumer{}, Blockers: []string{}}
	turns := make(map[string]retentionTurn, len(source))
	for key, value := range source {
		turns[key] = value
	}
	// Bind the reviewed ID to the actual dependency records, including versions.
	// All reads and the final prepare CAS use the same native authority view.
	evidence := make(map[string]json.RawMessage)
	read := func(kind string, out any) error {
		var raws []json.RawMessage
		if err := tx.List(recordKey(kind, ""), func(key string, raw json.RawMessage) error {
			evidence[key] = raw
			raws = append(raws, raw)
			return nil
		}); err != nil {
			return err
		}
		raw, err := json.Marshal(raws)
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, out)
	}
	var agents []orchestration.AgentSnapshot
	var flows []agentflow.Run
	var tasks []orchestration.TaskSnapshot
	var waits []orchestration.WaitSet
	var messages []orchestration.Message
	var completions []completionRecord
	var merges []workspacecoord.MergeRequest
	for _, item := range []struct {
		kind string
		out  any
	}{
		{"agent", &agents}, {"flow", &flows},
		{"task", &tasks}, {"wait", &waits}, {"message", &messages},
		{"agent-completion", &completions}, {"merge", &merges},
	} {
		if err := read(item.kind, item.out); err != nil {
			return plan, err
		}
	}
	var team orchestration.TeamID
	var actor orchestration.AgentID
	for _, a := range agents {
		if a.SessionID != id {
			continue
		}
		if actor != "" && actor != a.ID {
			return plan, errors.New("ambiguous session owner")
		}
		team, actor = a.TeamID, a.ID
		if a.ParentAgentID != "" && (!isTerminalAgentState(a.State) || a.Result == nil || a.Result.Validate() != nil) {
			plan.Blockers = append(plan.Blockers, "child Agent has not durably finished with a valid result")
		}
	}
	for _, a := range agents {
		if team == "" || a.TeamID != team {
			continue
		}
		if a.ParentAgentID != "" && !isTerminalAgentState(a.State) {
			plan.Blockers = append(plan.Blockers, "nonterminal child Agent")
		}
		for _, c := range completions {
			if c.Completion.AgentID == a.ID && !c.Settled {
				plan.Blockers = append(plan.Blockers, "unsettled Agent completion")
			}
		}
	}
	localByRef := make(map[tool.SnapshotReference]string)
	for _, c := range local {
		if err := c.Reference.Validate(); err != nil {
			return plan, err
		}
		if previous, exists := localByRef[c.Reference]; exists && previous != c.SnapshotID {
			return plan, tool.ErrSnapshotReferenceConflict
		}
		localByRef[c.Reference] = c.SnapshotID
		if c.Reference.Kind == "turn" && c.Reference.Scope == string(id) {
			if _, exists := turns[c.Reference.ID]; !exists {
				turns[c.Reference.ID] = retentionTurn{}
			}
		}
	}
	add := func(c retention.Consumer, terminal, closed bool) error {
		var receiptID string
		prepared, err := tx.Get(recordKey("retention-consumer", retentionReferenceKey(c.Reference)), &receiptID)
		if err != nil {
			return err
		}
		if prepared {
			record, err := getRecord[retentionRecord](tx, "retention-receipt", receiptID)
			if err != nil {
				return err
			}
			if !record.Complete {
				plan.Blockers = append(plan.Blockers, "consumer has prepared retention: "+c.Reference.ID)
			}
			return nil
		}
		if closed {
			return nil
		}
		if localID := localByRef[c.Reference]; localID != "" {
			if c.SnapshotID != "" && c.SnapshotID != localID {
				return tool.ErrSnapshotReferenceConflict
			}
			c.SnapshotID = localID
		}
		if !terminal {
			plan.Blockers = append(plan.Blockers, "unfinished or unverifiable "+c.Reference.Kind+" "+c.Reference.ID)
			return nil
		}
		plan.Consumers = append(plan.Consumers, c)
		return nil
	}
	if len(turns) > 10000 {
		return plan, errors.New("retention exceeds 10000 turns")
	}
	for turnID, state := range turns {
		terminal := state.started && state.completed
		if err := add(retention.Consumer{Reference: tool.SnapshotReference{Kind: "turn", Scope: string(id), ID: turnID}, SnapshotID: state.snapshot}, terminal, state.closed); err != nil {
			return plan, err
		}
	}
	for _, f := range flows {
		terminal := f.State == agentflow.StateCompleted || f.State == agentflow.StateFailed || f.State == agentflow.StateCanceled
		if team != "" && f.TeamID == team && !terminal {
			plan.Blockers = append(plan.Blockers, "nonterminal team Flow")
		}
		if f.SessionID != id {
			continue
		}
		ref := tool.SnapshotReference{Kind: "flow", Scope: string(f.TeamID), ID: f.IdempotencyKey}
		if err := ref.Validate(); err != nil {
			return plan, err
		}
		if err := add(retention.Consumer{Reference: ref, SnapshotID: f.RuntimeSnapshotID}, terminal, false); err != nil {
			return plan, err
		}
	}
	for _, t := range tasks {
		if team != "" && t.TeamID == team && !taskTerminal(t.State) {
			plan.Blockers = append(plan.Blockers, "nonterminal Task")
		}
	}
	for _, w := range waits {
		if team != "" && w.TeamID == team && (w.State == orchestration.WaitSetPending || w.State == orchestration.WaitSetResolved) {
			plan.Blockers = append(plan.Blockers, "unconsumed wait")
		}
	}
	for _, m := range messages {
		if actor != "" && m.RecipientAgentID == actor && m.ConsumerTurnID != "" && m.DeliveryState != orchestration.DeliveryAcknowledged {
			plan.Blockers = append(plan.Blockers, "unacknowledged turn mailbox delivery")
		}
	}
	for _, m := range merges {
		if team == "" || m.TeamID != team {
			continue
		}
		if m.State != workspacecoord.MergeApplied && m.State != workspacecoord.MergeRejected && m.State != workspacecoord.MergeSuperseded {
			plan.Blockers = append(plan.Blockers, "unfinished merge")
		}
		if m.State == workspacecoord.MergeApplied {
			var final workspacecoord.MergeFinalization
			found, err := tx.Get(recordKey(mergeFinalizationKind, string(m.ID)), &final)
			if err != nil {
				return plan, err
			}
			if !found || !final.Settled {
				plan.Blockers = append(plan.Blockers, "unsettled merge finalization")
			}
			raw, _ := json.Marshal(final)
			evidence["merge-finalization/"+string(m.ID)] = raw
		}
	}
	sort.Slice(plan.Consumers, func(i, j int) bool {
		a, b := plan.Consumers[i].Reference, plan.Consumers[j].Reference
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Scope != b.Scope {
			return a.Scope < b.Scope
		}
		return a.ID < b.ID
	})
	sort.Strings(plan.Blockers)
	raw, err := json.Marshal(struct {
		Plan     retention.Plan
		Evidence any
	}{plan, evidence})
	if err != nil {
		return plan, err
	}
	plan.ID = retention.Digest(raw)
	return plan, nil
}

func retentionReferenceKey(ref tool.SnapshotReference) string {
	raw, _ := json.Marshal(ref)
	return retention.Digest(raw)
}

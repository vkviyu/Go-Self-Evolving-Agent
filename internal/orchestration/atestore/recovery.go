package atestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentflow"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

type abandonmentRecord struct {
	ID       string
	Receipt  session.Abandonment
	TeamID   orchestration.TeamID
	AgentID  orchestration.AgentID
	Sequence uint64
	Events   []session.PendingEvent
	Complete bool
}

func abandonmentKey(parts ...string) string {
	raw, _ := json.Marshal(parts)
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

// AbandonRecovery closes one interrupted Turn after durable evidence has been
// inspected. It never claims that an external effect was undone.
func (store *Store) AbandonRecovery(ctx context.Context, request session.AbandonRequest) (session.Abandonment, error) {
	if err := request.Validate(); err != nil {
		return session.Abandonment{}, err
	}
	if store.sessions == nil {
		return session.Abandonment{}, errors.New("authoritative session events are unavailable")
	}
	record, err := store.prepareAbandonment(ctx, request)
	if err != nil {
		return session.Abandonment{}, err
	}
	if err := store.finishAbandonment(ctx, record); err != nil {
		return session.Abandonment{}, err
	}
	return record.Receipt, nil
}

func (store *Store) ResumeAbandonments(ctx context.Context, id session.ID) error {
	if id == "" {
		return errors.New("abandonment recovery requires an owned session")
	}
	var records []abandonmentRecord
	if err := store.authority.View(ctx, func(tx *ateauthority.Tx) error {
		var err error
		records, err = listRecords[abandonmentRecord](tx, "recovery-abandonment")
		return err
	}); err != nil {
		return err
	}
	for _, record := range records {
		if record.Receipt.SessionID == id && !record.Complete {
			if err := store.finishAbandonment(ctx, record); err != nil {
				return fmt.Errorf("resume abandonment %s: %w", record.ID, err)
			}
		}
	}
	return nil
}

// CheckSnapshotReference is the authority guard for runtime snapshot Save.
func (store *Store) CheckSnapshotReference(ctx context.Context, ref tool.SnapshotReference) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if err := store.CheckRetentionReference(ctx, ref); err != nil {
		return err
	}
	if ref.Kind == "flow" {
		return nil
	}
	var closed bool
	if err := store.authority.View(ctx, func(tx *ateauthority.Tx) error {
		var receiptID string
		var err error
		closed, err = tx.Get(recordKey("recovery-abandoned-turn", abandonmentKey(ref.Scope, ref.ID)), &receiptID)
		return err
	}); err != nil {
		return err
	}
	if closed {
		return tool.ErrSnapshotReferenceReleased
	}
	if store.sessions == nil {
		return errors.New("authoritative session events are unavailable")
	}
	err := session.CheckRecovery(ctx, store.sessions, session.ID(ref.Scope), ref.ID)
	if errors.Is(err, session.ErrRecoveryAbandoned) || errors.Is(err, session.ErrRecoveryArchived) {
		return errors.Join(tool.ErrSnapshotReferenceReleased, err)
	}
	return err
}

func (store *Store) prepareAbandonment(ctx context.Context, request session.AbandonRequest) (abandonmentRecord, error) {
	var record abandonmentRecord
	record.ID = abandonmentKey(string(request.SessionID), "turn", request.TurnID)
	err := store.authority.View(ctx, func(tx *ateauthority.Tx) error {
		existing, err := getRecord[abandonmentRecord](tx, "recovery-abandonment", record.ID)
		if err == nil {
			record = existing
			return matchAbandonment(request, record)
		}
		if !errors.Is(err, orchestration.ErrNotFound) {
			return err
		}
		record.Receipt = session.Abandonment{
			SessionID:   request.SessionID,
			Reason:      request.Reason,
			AbandonedAt: time.Now().UTC(),
			TurnIDs:     []string{request.TurnID},
		}
		return nil
	})
	if err != nil || record.Complete || record.Events != nil {
		return record, err
	}
	state := &abandonTurn{}
	err = session.VisitEvents(ctx, store.sessions, request.SessionID, func(event session.Event) error {
		record.Sequence = event.Sequence
		if event.CorrelationID != request.TurnID {
			return nil
		}
		return state.observe(event)
	})
	if err != nil {
		return record, err
	}
	if state.closed {
		return record, session.ErrRecoveryAbandoned
	}
	if !state.started || state.completed {
		return record, errors.New("turn is missing or already completed")
	}
	record.Events = []session.PendingEvent{{
		Type: session.EventTurnAbandoned, CorrelationID: request.TurnID,
		CausationID: record.ID, RuntimeSnapshotID: state.snapshot,
		Payload: session.MustJSONPayload(map[string]any{
			"turn_id": request.TurnID, "reason": request.Reason,
			"effects_reverted": false, "effects_reviewed": true,
		}),
	}}
	err = store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		existing, err := getRecord[abandonmentRecord](tx, "recovery-abandonment", record.ID)
		if err == nil {
			record = existing
			return matchAbandonment(request, record)
		}
		if !errors.Is(err, orchestration.ErrNotFound) {
			return err
		}
		if err := guardAbandonment(tx, &record); err != nil {
			return err
		}
		if err := createRecord(tx, "recovery-abandoned-turn", abandonmentKey(string(request.SessionID), request.TurnID), record.ID); err != nil {
			return err
		}
		return createRecord(tx, "recovery-abandonment", record.ID, record)
	})
	return record, err
}

func matchAbandonment(request session.AbandonRequest, record abandonmentRecord) error {
	if record.Receipt.SessionID != request.SessionID || record.Receipt.Reason != request.Reason ||
		len(record.Receipt.TurnIDs) != 1 || record.Receipt.TurnIDs[0] != request.TurnID {
		return orchestration.ErrIdempotencyConflict
	}
	return nil
}

type abandonTurn struct {
	started, completed, closed bool
	snapshot                   string
}

func (state *abandonTurn) observe(event session.Event) error {
	switch event.Type {
	case session.EventTurnStarted:
		if state.started || state.completed || state.closed {
			return errors.New("inconsistent turn start")
		}
		state.started, state.snapshot = true, event.RuntimeSnapshotID
	case session.EventTurnCompleted:
		if !state.started || state.completed || (event.RuntimeSnapshotID != "" && state.snapshot != "" && event.RuntimeSnapshotID != state.snapshot) {
			return errors.New("inconsistent turn completion evidence")
		}
		state.completed = true
	case session.EventTurnAbandoned, session.EventTurnArchived:
		state.closed = true
	}
	return nil
}

func (store *Store) finishAbandonment(ctx context.Context, record abandonmentRecord) error {
	if record.Complete {
		return nil
	}
	if store.sessions == nil || len(record.Events) == 0 {
		return errors.New("abandonment receipt has no authoritative event plan")
	}
	for i, pending := range record.Events {
		sequence := record.Sequence + uint64(i)
		items, err := store.sessions.Load(ctx, record.Receipt.SessionID, sequence, 1)
		if err != nil {
			return err
		}
		if len(items) > 0 {
			e := items[0]
			if e.Sequence != sequence+1 || e.Type != pending.Type || e.CausationID != pending.CausationID ||
				e.CorrelationID != pending.CorrelationID || e.RuntimeSnapshotID != pending.RuntimeSnapshotID || !bytes.Equal(e.Payload, pending.Payload) {
				return fmt.Errorf("%w: abandonment event plan diverged", session.ErrSequenceConflict)
			}
			continue
		}
		if _, err := store.sessions.Append(ctx, record.Receipt.SessionID, sequence, pending); err != nil {
			return err
		}
	}
	return store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		current, err := getRecord[abandonmentRecord](tx, "recovery-abandonment", record.ID)
		if err != nil || current.Complete {
			return err
		}
		if err := guardAbandonment(tx, &current); err != nil {
			return err
		}
		current.Complete = true
		return putRecord(tx, "recovery-abandonment", current.ID, current)
	})
}

// Refuse to take lifecycle ownership away from agents, mailbox deliveries,
// tasks, waits, Flows or merges. Unknown effects remain unknown.
func guardAbandonment(tx *ateauthority.Tx, record *abandonmentRecord) error {
	blocked := func(reason string) error { return fmt.Errorf("%w: %s", session.ErrRecoveryDependency, reason) }
	agents, err := listRecords[orchestration.AgentSnapshot](tx, "agent")
	if err != nil {
		return err
	}
	for _, agent := range agents {
		if agent.SessionID == record.Receipt.SessionID {
			if agent.ParentAgentID != "" {
				return blocked("child recovery belongs to Supervisor")
			}
			if record.TeamID != "" && (record.TeamID != agent.TeamID || record.AgentID != agent.ID) {
				return orchestration.ErrSequenceConflict
			}
			record.TeamID, record.AgentID = agent.TeamID, agent.ID
		}
	}
	for _, agent := range agents {
		if record.TeamID != "" && agent.TeamID == record.TeamID && agent.ParentAgentID != "" &&
			agent.State != orchestration.AgentCompleted && agent.State != orchestration.AgentFailed && agent.State != orchestration.AgentCanceled {
			return blocked("nonterminal child Agent")
		}
	}
	completions, err := listRecords[completionRecord](tx, "agent-completion")
	if err != nil {
		return err
	}
	for _, completion := range completions {
		if completion.Settled {
			continue
		}
		for _, agent := range agents {
			if record.TeamID != "" && agent.TeamID == record.TeamID && agent.ID == completion.Completion.AgentID {
				return blocked("unsettled Agent completion")
			}
		}
	}
	tasks, err := listRecords[orchestration.TaskSnapshot](tx, "task")
	if err != nil {
		return err
	}
	for _, task := range tasks {
		if record.TeamID != "" && task.TeamID == record.TeamID && task.State != orchestration.TaskCompleted && task.State != orchestration.TaskFailed && task.State != orchestration.TaskCanceled {
			return blocked("nonterminal Task")
		}
	}
	flows, err := listRecords[agentflow.Run](tx, "flow")
	if err != nil {
		return err
	}
	for _, flow := range flows {
		if (flow.SessionID == record.Receipt.SessionID || (record.TeamID != "" && flow.TeamID == record.TeamID)) &&
			flow.State != agentflow.StateCompleted && flow.State != agentflow.StateFailed && flow.State != agentflow.StateCanceled {
			return blocked("nonterminal Flow")
		}
	}
	messages, err := listRecords[orchestration.Message](tx, "message")
	if err != nil {
		return err
	}
	for _, message := range messages {
		if message.RecipientAgentID == record.AgentID && record.AgentID != "" && slices.Contains(record.Receipt.TurnIDs, message.ConsumerTurnID) &&
			message.DeliveryState != orchestration.DeliveryAcknowledged {
			return blocked("mailbox delivery still bound to target turn")
		}
	}
	waits, err := listRecords[orchestration.WaitSet](tx, "wait")
	if err != nil {
		return err
	}
	for _, wait := range waits {
		if record.TeamID != "" && wait.TeamID == record.TeamID && (wait.State == orchestration.WaitSetPending || wait.State == orchestration.WaitSetResolved) {
			return blocked("unconsumed wait")
		}
	}
	merges, err := listRecords[workspacecoord.MergeRequest](tx, "merge")
	if err != nil {
		return err
	}
	for _, merge := range merges {
		if record.TeamID == "" || merge.TeamID != record.TeamID {
			continue
		}
		if merge.State != workspacecoord.MergeApplied && merge.State != workspacecoord.MergeRejected && merge.State != workspacecoord.MergeSuperseded {
			return blocked("unfinished merge")
		}
		if merge.State == workspacecoord.MergeApplied {
			finalization, err := getRecord[workspacecoord.MergeFinalization](tx, mergeFinalizationKind, string(merge.ID))
			if errors.Is(err, orchestration.ErrNotFound) {
				return blocked("applied merge has no finalization proof")
			}
			if err != nil {
				return err
			}
			if !finalization.Settled {
				return blocked("unsettled merge finalization")
			}
		}
	}
	return nil
}

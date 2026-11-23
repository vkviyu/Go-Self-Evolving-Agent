package atestore

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

func TestCoreMailboxLedgerAndExactIdempotency(t *testing.T) {
	f := newCoreFixture(t)
	s := f.store
	ctx := context.Background()
	root := coreRoot(t, s, "mailbox")
	child := coreChild(t, s, root)
	spec := orchestration.MessageSpec{TeamID: root.TeamID, SenderAgentID: root.AgentID, RecipientAgentID: child.ID, Type: orchestration.MessageDirective, Body: json.RawMessage("{ \"text\": \"work\" }"), IdempotencyKey: "message"}
	first, err := s.Enqueue(ctx, spec)
	coreMust(t, err)
	f.reopen(t)
	retry, err := s.Enqueue(ctx, spec)
	coreMust(t, err)
	if retry.ID != first.ID || string(retry.Body) != string(spec.Body) {
		t.Fatal("message bytes or identity changed")
	}
	spec.Body = json.RawMessage(`{"text":"work"}`)
	_, err = s.Enqueue(ctx, spec)
	if !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("byte-distinct request accepted: %v", err)
	}
	_, err = s.MarkAcknowledged(ctx, first.ID)
	if !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("ack before delivery: %v", err)
	}
	claim := orchestration.MessageClaim{TeamID: root.TeamID, RecipientAgentID: child.ID, TurnID: "turn-1", Request: 2}
	bound, err := s.ClaimMessages(ctx, claim)
	coreMust(t, err)
	if len(bound) != 1 || bound[0].ConsumerTurnID != "turn-1" || bound[0].DeliveredAt == nil {
		t.Fatalf("claim: %+v", bound)
	}
	claim.TurnID = "turn-2"
	empty, err := s.ClaimMessages(ctx, claim)
	coreMust(t, err)
	if len(empty) != 0 {
		t.Fatal("another turn stole bound message")
	}
	count, err := s.ReleaseTurn(ctx, root.TeamID, child.ID, "turn-1")
	coreMust(t, err)
	if count != 1 {
		t.Fatal("release missed message")
	}
	bound, err = s.ClaimMessages(ctx, claim)
	coreMust(t, err)
	if len(bound) != 1 || bound[0].ConsumerTurnID != "turn-2" {
		t.Fatal("release did not make message available")
	}
	count, err = s.AcknowledgeTurn(ctx, root.TeamID, child.ID, "turn-2")
	coreMust(t, err)
	if count != 1 {
		t.Fatal("ack missed message")
	}
	count, err = s.AcknowledgeTurn(ctx, root.TeamID, child.ID, "turn-2")
	coreMust(t, err)
	if count != 0 {
		t.Fatal("ack repeated side effects")
	}
	_, err = s.MarkDelivered(ctx, first.ID)
	if !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatal("acknowledged message redelivered")
	}
	spec.IdempotencyKey = "message-2"
	second, err := s.Enqueue(ctx, spec)
	coreMust(t, err)
	if second.Sequence != 2 {
		t.Fatalf("failed requests consumed sequence: %d", second.Sequence)
	}
}

func TestCoreCompletionNotificationAtomicityAndFence(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	root := coreRoot(t, s, "completion")
	child := coreChild(t, s, root)
	run, err := orchestration.NewRunID()
	coreMust(t, err)
	running, err := s.TransitionAgent(ctx, orchestration.AgentTransition{AgentID: child.ID, Expected: child.Version, Target: orchestration.AgentRunning, RunID: run})
	coreMust(t, err)
	result := orchestration.AgentResult{Status: orchestration.ResultSucceeded, Summary: "done"}
	completion := orchestration.AgentCompletion{AgentID: child.ID, RunID: run, Result: result}
	_, err = s.PrepareAgentCompletion(ctx, completion)
	coreMust(t, err)
	terminal := running
	terminal.State = orchestration.AgentCompleted
	terminal.Result = &result
	notification, notify, err := orchestration.ResultMessage(terminal)
	coreMust(t, err)
	if !notify {
		t.Fatal("missing expected parent notification")
	}
	bad := notification
	bad.Body = json.RawMessage(`{"conflict":true}`)
	message, err := s.Enqueue(ctx, bad)
	coreMust(t, err)
	before, err := s.CurrentSequence(ctx, orchestration.StreamAgent, string(child.ID))
	coreMust(t, err)
	_, err = s.TransitionAgent(ctx, orchestration.AgentTransition{AgentID: child.ID, Expected: running.Version, Target: orchestration.AgentCompleted, Result: &result})
	if !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("conflicting notification accepted: %v", err)
	}
	unchanged, err := s.GetAgent(ctx, child.ID)
	coreMust(t, err)
	after, err := s.CurrentSequence(ctx, orchestration.StreamAgent, string(child.ID))
	coreMust(t, err)
	if unchanged.State != orchestration.AgentRunning || unchanged.Version != running.Version || before != after {
		t.Fatal("notification failure partially committed agent/events")
	}
	// Repair the deliberately corrupted notification fixture in native ATE.
	coreMust(t, s.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		message.Body = notification.Body
		return putRecord(tx, "message", string(message.ID), message)
	}))
	completed, err := s.TransitionAgent(ctx, orchestration.AgentTransition{AgentID: child.ID, Expected: running.Version, Target: orchestration.AgentCompleted, Result: &result})
	coreMust(t, err)
	_, err = s.TransitionAgent(ctx, orchestration.AgentTransition{AgentID: child.ID, Expected: completed.Version, Target: orchestration.AgentQueued})
	if !errors.Is(err, orchestration.ErrAgentStateConflict) {
		t.Fatal("unsettled completion allowed restart")
	}
	pending, err := s.PendingAgentCompletions(ctx, root.TeamID)
	coreMust(t, err)
	if len(pending) != 1 || pending[0].RunID != run {
		t.Fatalf("pending: %+v", pending)
	}
	coreMust(t, s.SettleAgentCompletion(ctx, child.ID, run))
	coreMust(t, s.SettleAgentCompletion(ctx, child.ID, run))
	queued, err := s.TransitionAgent(ctx, orchestration.AgentTransition{AgentID: child.ID, Expected: completed.Version, Target: orchestration.AgentQueued})
	coreMust(t, err)
	nextRun, err := orchestration.NewRunID()
	coreMust(t, err)
	_, err = s.TransitionAgent(ctx, orchestration.AgentTransition{AgentID: child.ID, Expected: queued.Version, Target: orchestration.AgentRunning, RunID: nextRun})
	coreMust(t, err)
	_, err = s.PrepareAgentCompletion(ctx, completion)
	if !errors.Is(err, orchestration.ErrAgentStateConflict) {
		t.Fatal("stale run result accepted")
	}
}

func TestCoreWaitResumeIdempotencyAndCancellation(t *testing.T) {
	f := newCoreFixture(t)
	s := f.store
	ctx := context.Background()
	root := coreRoot(t, s, "wait")
	child := coreChild(t, s, root)
	run, err := orchestration.NewRunID()
	coreMust(t, err)
	_, err = s.TransitionAgent(ctx, orchestration.AgentTransition{AgentID: root.AgentID, Expected: 1, Target: orchestration.AgentRunning, RunID: run})
	coreMust(t, err)
	deadline := time.Now().UTC().Add(time.Minute)
	spec := orchestration.WaitSpec{TeamID: root.TeamID, AgentID: root.AgentID, RunID: run, Condition: "children", TargetAgentIDs: []orchestration.AgentID{child.ID}, Deadline: &deadline, IdempotencyKey: "wait", ResumeToken: "resume"}
	wait, err := s.CreateWaitSet(ctx, spec)
	coreMust(t, err)
	f.reopen(t)
	again, err := s.CreateWaitSet(ctx, spec)
	coreMust(t, err)
	if again.ID != wait.ID {
		t.Fatal("wait retry duplicated suspension")
	}
	spec.Any = true
	_, err = s.CreateWaitSet(ctx, spec)
	if !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatal("changed wait accepted")
	}
	resolved, err := s.ResolveWaitSet(ctx, wait.ID, "ready")
	coreMust(t, err)
	if resolved.ResolvedAt == nil || resolved.State != orchestration.WaitSetResolved {
		t.Fatalf("resolved: %+v", resolved)
	}
	agent, err := s.GetAgent(ctx, root.AgentID)
	coreMust(t, err)
	if agent.State != orchestration.AgentQueued || agent.Activity != "wait_resolved" {
		t.Fatalf("not queued: %+v", agent)
	}
	consumed, err := s.ConsumeWaitSet(ctx, wait.ID, "dispatched")
	coreMust(t, err)
	if consumed.State != orchestration.WaitSetConsumed {
		t.Fatal("wait not consumed")
	}
	_, err = s.TransitionAgent(ctx, orchestration.AgentTransition{AgentID: agent.ID, Expected: agent.Version, Target: orchestration.AgentRunning, RunID: run})
	coreMust(t, err)
	spec.IdempotencyKey = "wait-2"
	wait, err = s.CreateWaitSet(ctx, spec)
	coreMust(t, err)
	coreMust(t, s.CancelWaitSetsForAgent(ctx, root.AgentID, "cancel"))
	wait, err = s.GetWaitSet(ctx, wait.ID)
	coreMust(t, err)
	if wait.State != orchestration.WaitSetCanceled {
		t.Fatal("pending wait not canceled")
	}
}

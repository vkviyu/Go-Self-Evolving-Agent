package sqlitestore

import (
	"context"
	"errors"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

func TestWaitSetSuspendsAndResolvesAgentAtomically(t *testing.T) {
	database, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: "wait-root"})
	if err != nil {
		t.Fatal(err)
	}
	waiter, err := store.CreateChild(context.Background(), orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker-v1"})
	if err != nil {
		t.Fatal(err)
	}
	target, err := store.CreateChild(context.Background(), orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker-v1"})
	if err != nil {
		t.Fatal(err)
	}
	runID, err := orchestration.NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	waiter, err = store.TransitionAgent(context.Background(), orchestration.AgentTransition{AgentID: waiter.ID, Expected: waiter.Version, Target: orchestration.AgentRunning, RunID: runID, Activity: "runner"})
	if err != nil {
		t.Fatal(err)
	}
	waitSet, err := store.CreateWaitSet(context.Background(), orchestration.WaitSpec{
		TeamID: root.TeamID, AgentID: waiter.ID, RunID: runID, ToolCallID: "wait-call-1", Condition: "children",
		TargetAgentIDs: []orchestration.AgentID{target.ID}, IdempotencyKey: "wait-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if waitSet.State != orchestration.WaitSetPending || waitSet.AgentID != waiter.ID {
		t.Fatalf("unexpected wait set: %#v", waitSet)
	}
	persisted, err := store.GetAgent(context.Background(), waiter.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != orchestration.AgentWaiting || persisted.Activity != "waiting:children" {
		t.Fatalf("waiter was not suspended: %#v", persisted)
	}
	repeated, err := store.CreateWaitSet(context.Background(), orchestration.WaitSpec{
		TeamID: root.TeamID, AgentID: waiter.ID, RunID: runID, ToolCallID: "wait-call-1", Condition: "children",
		TargetAgentIDs: []orchestration.AgentID{target.ID}, IdempotencyKey: "wait-1",
	})
	if err != nil || repeated.ID != waitSet.ID {
		t.Fatalf("wait set idempotency failed: %#v err=%v", repeated, err)
	}
	if _, err := store.CreateWaitSet(context.Background(), orchestration.WaitSpec{
		TeamID: root.TeamID, AgentID: waiter.ID, RunID: runID, Condition: "different",
		TargetAgentIDs: []orchestration.AgentID{target.ID}, IdempotencyKey: "wait-1",
	}); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("got %v, want wait idempotency conflict", err)
	}
	resolved, err := store.ResolveWaitSet(context.Background(), waitSet.ID, "child finished")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.State != orchestration.WaitSetResolved || resolved.ResolvedAt == nil {
		t.Fatalf("wait set was not resolved: %#v", resolved)
	}
	persisted, err = store.GetAgent(context.Background(), waiter.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != orchestration.AgentQueued || persisted.Activity != "wait_resolved" {
		t.Fatalf("waiter was not requeued: %#v", persisted)
	}
	consumed, err := store.ConsumeWaitSet(context.Background(), waitSet.ID, "resume dispatched")
	if err != nil {
		t.Fatal(err)
	}
	if consumed.State != orchestration.WaitSetConsumed {
		t.Fatalf("wait set was not marked consumed: %#v", consumed)
	}
	repeatedConsumed, err := store.ConsumeWaitSet(context.Background(), waitSet.ID, "retry")
	if err != nil || repeatedConsumed.State != orchestration.WaitSetConsumed {
		t.Fatalf("consuming an already consumed wait set was not idempotent: %#v err=%v", repeatedConsumed, err)
	}
}

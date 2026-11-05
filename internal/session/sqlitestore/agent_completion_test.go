package sqlitestore

import (
	"context"
	"errors"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

func TestAgentCompletionFencesRunAndRejectsCorruptReceipt(t *testing.T) {
	database, root, _ := refinementFixture(t)
	ctx, store := context.Background(), database.Orchestration()
	child, err := store.CreateChild(ctx, orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := orchestration.NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	child, err = store.TransitionAgent(ctx, orchestration.AgentTransition{AgentID: child.ID, Expected: child.Version, Target: orchestration.AgentRunning, RunID: run})
	if err != nil {
		t.Fatal(err)
	}
	result := orchestration.AgentResult{Status: orchestration.ResultSucceeded, Summary: "frozen evidence"}
	receipt := orchestration.AgentCompletion{AgentID: child.ID, RunID: run, Result: result}
	if _, err := store.PrepareAgentCompletion(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PrepareAgentCompletion(ctx, receipt); err != nil {
		t.Fatalf("identical receipt: %v", err)
	}
	changed := receipt
	changed.Result.Summary = "altered"
	if _, err := store.PrepareAgentCompletion(ctx, changed); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("mutated frozen result: %v", err)
	}
	child, err = store.TransitionAgent(ctx, orchestration.AgentTransition{AgentID: child.ID, Expected: child.Version, Target: orchestration.AgentCompleted, RunID: run, Result: &result})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionAgent(ctx, orchestration.AgentTransition{AgentID: child.ID, Expected: child.Version, Target: orchestration.AgentQueued}); !errors.Is(err, orchestration.ErrAgentStateConflict) {
		t.Fatalf("continued before settlement: %v", err)
	}
	if err := store.SettleAgentCompletion(ctx, child.ID, run); err != nil {
		t.Fatal(err)
	}
	child, err = store.TransitionAgent(ctx, orchestration.AgentTransition{AgentID: child.ID, Expected: child.Version, Target: orchestration.AgentQueued})
	if err != nil {
		t.Fatal(err)
	}
	next, err := orchestration.NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.TransitionAgent(ctx, orchestration.AgentTransition{AgentID: child.ID, Expected: child.Version, Target: orchestration.AgentRunning, RunID: next})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PrepareAgentCompletion(ctx, receipt); !errors.Is(err, orchestration.ErrAgentStateConflict) {
		t.Fatalf("old Run receipt accepted under new Run: %v", err)
	}
	if _, err := database.db.ExecContext(ctx, `UPDATE agent_completions SET result = ?, settled = 0 WHERE agent_id = ? AND run_id = ?`, []byte(`{"status":"succeeded","summary":"corrupt"}`), string(child.ID), string(run)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PendingAgentCompletions(ctx, root.TeamID); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("corrupt receipt accepted: %v", err)
	}
}

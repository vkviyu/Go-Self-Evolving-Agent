package sqlitestore

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

func TestTaskPlanEditPersistsAndFencesStaleDispatch(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "plan.db")
	database, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, orchestration.TaskSpec{TeamID: root.TeamID, Title: "original"})
	if err != nil {
		t.Fatal(err)
	}
	task, err = store.TransitionTask(ctx, orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskReady})
	if err != nil {
		t.Fatal(err)
	}
	old := task
	title := "updated contract"
	edit := orchestration.TaskUpdate{TaskID: task.ID, Expected: task.Version, ActorAgentID: root.AgentID, Title: &title, IdempotencyKey: "edit"}
	task, err = store.UpdateTask(ctx, edit)
	if err != nil {
		t.Fatal(err)
	}
	stale := orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker", TaskID: task.ID, TaskVersion: old.Version, InitialPrompt: old.Title, IdempotencyKey: "dispatch"}
	if _, err := store.CreateChild(ctx, stale); !errors.Is(err, orchestration.ErrTaskStateConflict) {
		t.Fatalf("stale dispatch: %v", err)
	}
	agents, err := store.ListAgents(ctx, orchestration.AgentQuery{TeamID: root.TeamID})
	if err != nil || len(agents) != 1 {
		t.Fatalf("stale dispatch leaked child: %v %v", agents, err)
	}
	conflicting := edit
	other := "different"
	conflicting.Title = &other
	if _, err := store.UpdateTask(ctx, conflicting); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("changed command replay: %v", err)
	}
	staleEdit := edit
	staleEdit.IdempotencyKey = "stale"
	if _, err := store.UpdateTask(ctx, staleEdit); !errors.Is(err, orchestration.ErrTaskStateConflict) {
		t.Fatalf("stale edit: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	store = database.Orchestration()
	replay, err := store.UpdateTask(ctx, edit)
	if err != nil || replay.Version != task.Version || replay.Title != title {
		t.Fatalf("reopen replay: %+v %v", replay, err)
	}
	stale.TaskVersion = task.Version
	stale.InitialPrompt = task.Title
	child, err := store.CreateChild(ctx, stale)
	if err != nil {
		t.Fatal(err)
	}
	replay, err = store.UpdateTask(ctx, edit)
	if err != nil || replay.AssigneeAgentID != child.ID {
		t.Fatalf("committed edit replay after claim: %+v %v", replay, err)
	}
	denied := edit
	denied.Expected = replay.Version
	denied.IdempotencyKey = "after-dispatch"
	if _, err := store.UpdateTask(ctx, denied); !errors.Is(err, orchestration.ErrTaskEditNotAllowed) {
		t.Fatalf("edit live contract: %v", err)
	}
}

func TestTaskPlanEventFailureRollsBackEditAndReceipt(t *testing.T) {
	database, root, task := refinementFixture(t)
	ctx, store := context.Background(), database.Orchestration()
	if _, err := database.db.ExecContext(ctx, `CREATE TRIGGER fail_plan_event BEFORE INSERT ON orchestration_events WHEN NEW.event_type = 'task.updated' BEGIN SELECT RAISE(ABORT, 'injected edit event failure'); END`); err != nil {
		t.Fatal(err)
	}
	title := "must roll back"
	edit := orchestration.TaskUpdate{TaskID: task.ID, Expected: task.Version, ActorAgentID: root.AgentID, Title: &title, IdempotencyKey: "atomic-edit"}
	if _, err := store.UpdateTask(ctx, edit); err == nil {
		t.Fatal("expected event failure")
	}
	unchanged, err := store.GetTask(ctx, task.ID)
	if err != nil || unchanged.Title != task.Title || unchanged.Version != task.Version {
		t.Fatalf("partial edit committed: %+v %v", unchanged, err)
	}
	if _, err := database.db.ExecContext(ctx, `DROP TRIGGER fail_plan_event`); err != nil {
		t.Fatal(err)
	}
	updated, err := store.UpdateTask(ctx, edit)
	if err != nil || updated.Version != task.Version+1 {
		t.Fatalf("failed transaction left receipt: %+v %v", updated, err)
	}
}

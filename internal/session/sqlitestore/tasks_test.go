package sqlitestore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

func TestTaskLifecycleDependenciesAndIdempotency(t *testing.T) {
	database, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: "task-lifecycle-root"})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := store.CreateChild(context.Background(), orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker-v1"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.CreateTask(context.Background(), orchestration.TaskSpec{
		TeamID: root.TeamID, Title: "Prepare", Description: "prepare inputs", Priority: 2, IdempotencyKey: "task-prepare",
	})
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := store.CreateTask(context.Background(), orchestration.TaskSpec{
		TeamID: root.TeamID, Title: "Prepare", Description: "prepare inputs", Priority: 2, IdempotencyKey: "task-prepare",
	})
	if err != nil || repeated.ID != first.ID {
		t.Fatalf("idempotent task create failed: repeated=%#v err=%v", repeated, err)
	}
	if _, err := store.CreateTask(context.Background(), orchestration.TaskSpec{
		TeamID: root.TeamID, Title: "Prepare differently", Description: "different", IdempotencyKey: "task-prepare",
	}); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("got %v, want task idempotency conflict", err)
	}
	second, err := store.CreateTask(context.Background(), orchestration.TaskSpec{
		TeamID: root.TeamID, ParentTaskID: first.ID, Title: "Verify", Description: "verify outputs",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddDependency(context.Background(), second.ID, first.ID, root.AgentID, "dependency-add"); err != nil {
		t.Fatal(err)
	}
	if err := store.AddDependency(context.Background(), first.ID, second.ID, root.AgentID, "dependency-cycle"); !errors.Is(err, orchestration.ErrTaskDependencyCycle) {
		t.Fatalf("got %v, want dependency cycle", err)
	}
	if _, err := store.TransitionTask(context.Background(), orchestration.TaskTransition{TaskID: first.ID, Expected: first.Version, Target: orchestration.TaskReady}); err != nil {
		t.Fatal(err)
	}
	blocked, err := store.GetTask(context.Background(), second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if blocked.State != orchestration.TaskBlocked {
		t.Fatalf("task with incomplete dependency = %s, want blocked", blocked.State)
	}
	if _, err := store.TransitionTask(context.Background(), orchestration.TaskTransition{TaskID: second.ID, Expected: blocked.Version, Target: orchestration.TaskReady}); !errors.Is(err, orchestration.ErrTaskNotReady) {
		t.Fatalf("got %v, want task not ready while dependency is incomplete", err)
	}
	secondCurrent, err := store.GetTask(context.Background(), second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimTask(context.Background(), second.ID, worker.ID, secondCurrent.Version); !errors.Is(err, orchestration.ErrTaskNotReady) {
		t.Fatalf("got %v, want task not ready", err)
	}
	firstReady, err := store.GetTask(context.Background(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	firstClaimed, err := store.ClaimTask(context.Background(), first.ID, worker.ID, firstReady.Version)
	if err != nil {
		t.Fatal(err)
	}
	firstProgress, err := store.TransitionTask(context.Background(), orchestration.TaskTransition{TaskID: first.ID, Expected: firstClaimed.Version, Target: orchestration.TaskInProgress, AssigneeAgentID: worker.ID, ActorAgentID: worker.ID})
	if err != nil {
		t.Fatal(err)
	}
	firstDone, err := store.TransitionTask(context.Background(), orchestration.TaskTransition{TaskID: first.ID, Expected: firstProgress.Version, Target: orchestration.TaskCompleted, ActorAgentID: worker.ID})
	if err != nil {
		t.Fatal(err)
	}
	if firstDone.State != orchestration.TaskCompleted || firstDone.AssigneeAgentID != worker.ID {
		t.Fatalf("unexpected completed task: %#v", firstDone)
	}
	secondCurrent, err = store.GetTask(context.Background(), second.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondCurrent, err = store.GetTask(context.Background(), second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if secondCurrent.State != orchestration.TaskReady {
		t.Fatalf("completed dependency did not unblock task: %#v", secondCurrent)
	}
	secondClaimed, err := store.ClaimTask(context.Background(), second.ID, worker.ID, secondCurrent.Version)
	if err != nil {
		t.Fatal(err)
	}
	if secondClaimed.State != orchestration.TaskClaimed || secondClaimed.AssigneeAgentID != worker.ID {
		t.Fatalf("unexpected claimed task: %#v", secondClaimed)
	}
	dependencies, err := store.ListDependencies(context.Background(), second.ID)
	if err != nil || len(dependencies) != 1 || dependencies[0] != first.ID {
		t.Fatalf("unexpected dependencies: %#v err=%v", dependencies, err)
	}
	if _, err := store.ReleaseTask(context.Background(), second.ID, worker.ID, secondClaimed.Version); err != nil {
		t.Fatal(err)
	}
	released, err := store.GetTask(context.Background(), second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if released.Attempt != 1 {
		t.Fatalf("task attempt = %d, want 1 after release", released.Attempt)
	}
	if err := store.RemoveDependency(context.Background(), second.ID, first.ID, root.AgentID, "dependency-remove"); err != nil {
		t.Fatal(err)
	}
	dependencies, err = store.ListDependencies(context.Background(), second.ID)
	if err != nil || len(dependencies) != 0 {
		t.Fatalf("dependency was not removed: %#v err=%v", dependencies, err)
	}
}

func TestTaskPlanUpdateIsCASIdempotentAndStopsAtExecution(t *testing.T) {
	database, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: "task-update-root"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(context.Background(), orchestration.TaskSpec{TeamID: root.TeamID, Title: "draft"})
	if err != nil {
		t.Fatal(err)
	}
	title := "edited"
	updated, err := store.UpdateTask(context.Background(), orchestration.TaskUpdate{TaskID: task.ID, Expected: task.Version, ActorAgentID: root.AgentID, Title: &title, IdempotencyKey: "edit-1"})
	if err != nil || updated.Title != title || updated.Version != task.Version+1 {
		t.Fatalf("update=%#v err=%v", updated, err)
	}
	repeated, err := store.UpdateTask(context.Background(), orchestration.TaskUpdate{TaskID: task.ID, Expected: task.Version, ActorAgentID: root.AgentID, Title: &title, IdempotencyKey: "edit-1"})
	if err != nil || repeated.Version != updated.Version {
		t.Fatalf("idempotent update=%#v err=%v", repeated, err)
	}
	ready, err := store.TransitionTask(context.Background(), orchestration.TaskTransition{TaskID: task.ID, Expected: updated.Version, Target: orchestration.TaskReady})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimTask(context.Background(), task.ID, root.AgentID, ready.Version); err != nil {
		t.Fatal(err)
	}
	description := "must reject"
	if _, err := store.UpdateTask(context.Background(), orchestration.TaskUpdate{TaskID: task.ID, Expected: ready.Version + 1, ActorAgentID: root.AgentID, Description: &description}); !errors.Is(err, orchestration.ErrTaskEditNotAllowed) {
		t.Fatalf("editing an executing task returned %v", err)
	}
}

func TestCreateChildClaimsAssignedTaskAtomically(t *testing.T) {
	database, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: "task-spawn-root"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(context.Background(), orchestration.TaskSpec{TeamID: root.TeamID, Title: "Spawned work"})
	if err != nil {
		t.Fatal(err)
	}
	task, err = store.TransitionTask(context.Background(), orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskReady})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.CreateChild(context.Background(), orchestration.SpawnSpec{
		TeamID: root.TeamID, ParentAgentID: root.AgentID, TaskID: task.ID, ProfileRevision: "worker-v1", InitialPrompt: "work from task",
	})
	if err != nil {
		t.Fatal(err)
	}
	if child.AssignedTaskID != task.ID {
		t.Fatalf("child task assignment = %s, want %s", child.AssignedTaskID, task.ID)
	}
	claimed, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.State != orchestration.TaskClaimed || claimed.AssigneeAgentID != child.ID {
		t.Fatalf("task was not atomically claimed: %#v", claimed)
	}
	if _, err := store.CreateChild(context.Background(), orchestration.SpawnSpec{
		TeamID: root.TeamID, ParentAgentID: root.AgentID, TaskID: task.ID, ProfileRevision: "worker-v1", InitialPrompt: "duplicate claim",
	}); err == nil {
		t.Fatal("second child unexpectedly claimed an already-assigned task")
	}
}

func TestTaskVerificationRequiresSeparateReviewer(t *testing.T) {
	database, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: "task-verification-root"})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := store.CreateChild(context.Background(), orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker-v1"})
	if err != nil {
		t.Fatal(err)
	}
	reviewer, err := store.CreateChild(context.Background(), orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "reviewer-v1"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(context.Background(), orchestration.TaskSpec{
		TeamID: root.TeamID, Title: "Reviewable change", VerificationProfile: "reviewer-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err = store.TransitionTask(context.Background(), orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskReady})
	if err != nil {
		t.Fatal(err)
	}
	task, err = store.ClaimTask(context.Background(), task.ID, worker.ID, task.Version)
	if err != nil {
		t.Fatal(err)
	}
	task, err = store.TransitionTask(context.Background(), orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskInProgress, AssigneeAgentID: worker.ID, ActorAgentID: worker.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionTask(context.Background(), orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskCompleted, ActorAgentID: worker.ID}); !errors.Is(err, orchestration.ErrTaskVerificationRequired) {
		t.Fatalf("worker completion got %v, want verification required", err)
	}
	task, err = store.TransitionTask(context.Background(), orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskVerification, AssigneeAgentID: worker.ID, ActorAgentID: worker.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionTask(context.Background(), orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskCompleted, ActorAgentID: worker.ID}); !errors.Is(err, orchestration.ErrTaskReviewerRequired) {
		t.Fatalf("worker self-review got %v, want reviewer required", err)
	}
	completed, err := store.TransitionTask(context.Background(), orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskCompleted, ActorAgentID: reviewer.ID})
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != orchestration.TaskCompleted || completed.AssigneeAgentID != worker.ID {
		t.Fatalf("reviewer completion changed task incorrectly: %#v", completed)
	}
}

func TestExpiredTaskClaimIsReapedOnlyAfterAgentStops(t *testing.T) {
	database, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: "task-claim-expiry-root"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(context.Background(), orchestration.TaskSpec{TeamID: root.TeamID, Title: "Expiring claim"})
	if err != nil {
		t.Fatal(err)
	}
	task, err = store.TransitionTask(context.Background(), orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskReady})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := store.CreateChild(context.Background(), orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, TaskID: task.ID, ProfileRevision: "worker-v1"})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ClaimExpiresAt == nil {
		t.Fatal("claimed task has no expiry")
	}
	future := claimed.ClaimExpiresAt.Add(time.Second)
	if count, err := store.ReapExpiredTaskClaims(context.Background(), future); err != nil || count != 0 {
		t.Fatalf("live Agent claim was reaped: count=%d err=%v", count, err)
	}
	if _, err := store.TransitionAgent(context.Background(), orchestration.AgentTransition{AgentID: worker.ID, Expected: worker.Version, Target: orchestration.AgentFailed, Activity: "failed", Reason: "worker stopped"}); err != nil {
		t.Fatal(err)
	}
	count, err := store.ReapExpiredTaskClaims(context.Background(), future)
	if err != nil || count != 1 {
		t.Fatalf("expired claim was not reaped: count=%d err=%v", count, err)
	}
	released, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if released.State != orchestration.TaskReady || released.AssigneeAgentID != "" || released.ClaimExpiresAt != nil || released.Attempt != 1 {
		t.Fatalf("unexpected reaped task: %#v", released)
	}
}

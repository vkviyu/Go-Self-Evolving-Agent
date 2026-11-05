package workspacecoord_test

import (
	"context"
	"errors"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/sqlitestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

type mergeGit struct {
	base string
}

func (git *mergeGit) Run(_ context.Context, _ string, arguments ...string) ([]byte, error) {
	if len(arguments) >= 2 && arguments[0] == "rev-parse" {
		return []byte(git.base + "\n"), nil
	}
	return []byte("applied\n"), nil
}

func TestMergeExecutorAppliesPendingPatchAndIsIdempotent(t *testing.T) {
	database, err := sqlitestore.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: "merge-executor-root"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.CreateChild(context.Background(), orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker-v1", InitialPrompt: "merge"})
	if err != nil {
		t.Fatal(err)
	}
	workspaceRoot := t.TempDir()
	view, err := store.CreateView(context.Background(), workspacecoord.ViewSpec{
		TeamID: root.TeamID, AgentID: child.ID, Mode: workspacecoord.ViewGitWorktree,
		WorkspaceRoot: workspaceRoot, RootPath: workspaceRoot, BaseRevision: "base-1", IdempotencyKey: "merge-view",
	})
	if err != nil {
		t.Fatal(err)
	}
	merge, err := store.EnqueueMerge(context.Background(), workspacecoord.MergeSpec{
		TeamID: root.TeamID, AgentID: child.ID, ViewID: view.ID, BaseRevision: "base-1", PatchArtifact: "patch-1", IdempotencyKey: "merge-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	git := &mergeGit{base: "base-1"}
	executor, err := workspacecoord.NewMergeExecutor(store, nil, &workspacecoord.WorktreeManager{Root: workspaceRoot, Git: git}, workspacecoord.ArtifactReaderFunc(func(context.Context, string) ([]byte, error) {
		return []byte("diff --git a/file b/file\n"), nil
	}), store)
	if err != nil {
		t.Fatal(err)
	}
	executor.AutoMerge = true // explicit trusted host grant
	results, err := executor.RunPending(context.Background(), root.TeamID, 1)
	if err != nil || len(results) != 1 || results[0].State != workspacecoord.MergeApplied {
		t.Fatalf("merge execution = %#v err=%v", results, err)
	}
	results, err = executor.RunPending(context.Background(), root.TeamID, 1)
	if err != nil || len(results) != 0 {
		t.Fatalf("repeated merge execution = %#v err=%v", results, err)
	}
	stored, err := store.GetMerge(context.Background(), merge.ID)
	if err != nil || stored.State != workspacecoord.MergeApplied {
		t.Fatalf("stored merge = %#v err=%v", stored, err)
	}
}

func TestMergeExecutorCreatesResolutionTaskOnConflict(t *testing.T) {
	database, err := sqlitestore.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: "merge-conflict-root"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.CreateChild(context.Background(), orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker-v1", InitialPrompt: "merge"})
	if err != nil {
		t.Fatal(err)
	}
	workspaceRoot := t.TempDir()
	view, err := store.CreateView(context.Background(), workspacecoord.ViewSpec{TeamID: root.TeamID, AgentID: child.ID, Mode: workspacecoord.ViewGitWorktree, WorkspaceRoot: workspaceRoot, RootPath: workspaceRoot, BaseRevision: "base-1", IdempotencyKey: "conflict-view"})
	if err != nil {
		t.Fatal(err)
	}
	merge, err := store.EnqueueMerge(context.Background(), workspacecoord.MergeSpec{TeamID: root.TeamID, AgentID: child.ID, ViewID: view.ID, BaseRevision: "base-1", PatchArtifact: "patch-1", IdempotencyKey: "conflict-merge"})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := workspacecoord.NewMergeExecutor(store, nil, &workspacecoord.WorktreeManager{Root: workspaceRoot, Git: &mergeGit{base: "different-base"}}, workspacecoord.ArtifactReaderFunc(func(context.Context, string) ([]byte, error) {
		return nil, errors.New("should not read after base mismatch")
	}), store)
	if err != nil {
		t.Fatal(err)
	}
	executor.AutoMerge = true
	results, err := executor.RunPending(context.Background(), root.TeamID, 0)
	if err != nil || len(results) != 1 || results[0].State != workspacecoord.MergeConflicted {
		t.Fatalf("conflict execution = %#v err=%v", results, err)
	}
	tasks, err := store.ListTasks(context.Background(), orchestration.TaskQuery{TeamID: root.TeamID})
	if err != nil || len(tasks) != 1 || tasks[0].Title == "" {
		t.Fatalf("resolution tasks = %#v err=%v", tasks, err)
	}
	if tasks[0].ParentTaskID != "" || tasks[0].State != orchestration.TaskDraft {
		t.Fatalf("resolution task metadata = %#v", tasks[0])
	}
	stored, err := store.GetMerge(context.Background(), merge.ID)
	if err != nil || stored.ResolutionTaskID == "" {
		t.Fatalf("resolution task link = %#v err=%v", stored, err)
	}
	_ = merge
}

func TestMergeExecutorResumesRunningMergeAfterRestart(t *testing.T) {
	database, err := sqlitestore.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: "merge-running-recovery-root"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.CreateChild(context.Background(), orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker-v1", InitialPrompt: "merge"})
	if err != nil {
		t.Fatal(err)
	}
	workspaceRoot := t.TempDir()
	view, err := store.CreateView(context.Background(), workspacecoord.ViewSpec{TeamID: root.TeamID, AgentID: child.ID, Mode: workspacecoord.ViewGitWorktree, WorkspaceRoot: workspaceRoot, RootPath: workspaceRoot, BaseRevision: "base-1", IdempotencyKey: "running-view"})
	if err != nil {
		t.Fatal(err)
	}
	merge, err := store.EnqueueMerge(context.Background(), workspacecoord.MergeSpec{TeamID: root.TeamID, AgentID: child.ID, ViewID: view.ID, BaseRevision: "base-1", PatchArtifact: "patch-1", IdempotencyKey: "running-merge"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.TransitionMerge(context.Background(), workspacecoord.MergeTransition{MergeID: merge.ID, Expected: merge.Version, Target: workspacecoord.MergeRunning, ActorAgentID: child.ID}); err != nil {
		t.Fatal(err)
	}
	executor, err := workspacecoord.NewMergeExecutor(store, nil, &workspacecoord.WorktreeManager{Root: workspaceRoot, Git: &mergeGit{base: "base-1"}}, workspacecoord.ArtifactReaderFunc(func(context.Context, string) ([]byte, error) { return []byte("patch"), nil }), store)
	if err != nil {
		t.Fatal(err)
	}
	executor.AutoMerge = true
	results, err := executor.RunPending(context.Background(), root.TeamID, 1)
	if err != nil || len(results) != 1 || results[0].State != workspacecoord.MergeApplied {
		t.Fatalf("running merge recovery = %#v err=%v", results, err)
	}
}

func TestMergeExecutorRequeuesOnlyCompletedResolutionTask(t *testing.T) {
	database, err := sqlitestore.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: "merge-resolution-root"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.CreateChild(context.Background(), orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker-v1", InitialPrompt: "merge"})
	if err != nil {
		t.Fatal(err)
	}
	originalTask, err := store.CreateTask(context.Background(), orchestration.TaskSpec{TeamID: root.TeamID, Title: "original merge task"})
	if err != nil {
		t.Fatal(err)
	}
	originalTask, err = store.TransitionTask(context.Background(), orchestration.TaskTransition{TaskID: originalTask.ID, Expected: originalTask.Version, Target: orchestration.TaskReady})
	if err != nil {
		t.Fatal(err)
	}
	originalTask, err = store.ClaimTask(context.Background(), originalTask.ID, child.ID, originalTask.Version)
	if err != nil {
		t.Fatal(err)
	}
	originalTask, err = store.TransitionTask(context.Background(), orchestration.TaskTransition{TaskID: originalTask.ID, Expected: originalTask.Version, Target: orchestration.TaskInProgress, AssigneeAgentID: child.ID, ActorAgentID: child.ID})
	if err != nil {
		t.Fatal(err)
	}
	workspaceRoot := t.TempDir()
	view, err := store.CreateView(context.Background(), workspacecoord.ViewSpec{TeamID: root.TeamID, AgentID: child.ID, Mode: workspacecoord.ViewGitWorktree, WorkspaceRoot: workspaceRoot, RootPath: workspaceRoot, BaseRevision: "base-1", IdempotencyKey: "resolution-view"})
	if err != nil {
		t.Fatal(err)
	}
	merge, err := store.EnqueueMerge(context.Background(), workspacecoord.MergeSpec{TeamID: root.TeamID, AgentID: child.ID, TaskID: originalTask.ID, ViewID: view.ID, BaseRevision: "base-1", PatchArtifact: "patch-1", IdempotencyKey: "resolution-merge"})
	if err != nil {
		t.Fatal(err)
	}
	originalTask, err = store.TransitionTask(context.Background(), orchestration.TaskTransition{TaskID: originalTask.ID, Expected: originalTask.Version, Target: orchestration.TaskAwaitingMerge, AssigneeAgentID: child.ID, ActorAgentID: child.ID})
	if err != nil {
		t.Fatal(err)
	}
	git := &mergeGit{base: "different-base"}
	executor, err := workspacecoord.NewMergeExecutor(store, nil, &workspacecoord.WorktreeManager{Root: workspaceRoot, Git: git}, workspacecoord.ArtifactReaderFunc(func(context.Context, string) ([]byte, error) { return []byte("patch"), nil }), store)
	if err != nil {
		t.Fatal(err)
	}
	executor.AutoMerge = true
	if _, err := executor.RunPending(context.Background(), root.TeamID, 0); err != nil {
		t.Fatal(err)
	}
	conflicted, err := store.GetMerge(context.Background(), merge.ID)
	if err != nil || conflicted.ResolutionTaskID == "" {
		t.Fatalf("conflicted merge = %#v err=%v", conflicted, err)
	}
	if _, err := store.TransitionMerge(context.Background(), workspacecoord.MergeTransition{MergeID: merge.ID, Expected: conflicted.Version, Target: workspacecoord.MergePending}); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("direct conflict requeue error = %v", err)
	}
	if _, err := executor.RequeueResolved(context.Background(), merge.ID, conflicted.ResolutionTaskID); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("draft resolution task error = %v", err)
	}
	resolution, err := store.GetTask(context.Background(), conflicted.ResolutionTaskID)
	if err != nil {
		t.Fatal(err)
	}
	resolution, err = store.TransitionTask(context.Background(), orchestration.TaskTransition{TaskID: resolution.ID, Expected: resolution.Version, Target: orchestration.TaskReady})
	if err != nil {
		t.Fatal(err)
	}
	resolution, err = store.ClaimTask(context.Background(), resolution.ID, child.ID, resolution.Version)
	if err != nil {
		t.Fatal(err)
	}
	resolution, err = store.TransitionTask(context.Background(), orchestration.TaskTransition{TaskID: resolution.ID, Expected: resolution.Version, Target: orchestration.TaskInProgress, AssigneeAgentID: child.ID, ActorAgentID: child.ID})
	if err != nil {
		t.Fatal(err)
	}
	resolution, err = store.TransitionTask(context.Background(), orchestration.TaskTransition{TaskID: resolution.ID, Expected: resolution.Version, Target: orchestration.TaskCompleted, ActorAgentID: child.ID})
	if err != nil {
		t.Fatal(err)
	}
	git.base = "base-1"
	results, err := executor.RunPending(context.Background(), root.TeamID, 1)
	if err != nil || len(results) != 1 || results[0].State != workspacecoord.MergeApplied {
		t.Fatalf("automatic resolved merge = %#v err=%v", results, err)
	}
}

package sqlitestore

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

func TestSnapshotMergeSupersedesAndJournalsApply(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "snapshot-merge-root"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.CreateChild(ctx, orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker-v1"})
	if err != nil {
		t.Fatal(err)
	}
	base := "sha256:base"
	view, err := store.CreateView(ctx, workspacecoord.ViewSpec{
		TeamID: root.TeamID, AgentID: child.ID, Mode: workspacecoord.ViewSnapshot, WorkspaceRoot: t.TempDir(),
		RootPath: filepath.Join(t.TempDir(), "snap-1", "work"), BaseRevision: base, IdempotencyKey: "view-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.EnqueueMerge(ctx, workspacecoord.MergeSpec{
		TeamID: root.TeamID, AgentID: child.ID, ViewID: view.ID, BaseRevision: base, Kind: workspacecoord.MergeSnapshot,
		CandidateDigest: "sha256:c1", PatchArtifact: "sha256:p1", IdempotencyKey: "merge-1",
	})
	if err != nil || first.Kind != workspacecoord.MergeSnapshot || first.CandidateDigest != "sha256:c1" {
		t.Fatalf("enqueue snapshot merge: %#v err=%v", first, err)
	}
	second, err := store.EnqueueMerge(ctx, workspacecoord.MergeSpec{
		TeamID: root.TeamID, AgentID: child.ID, ViewID: view.ID, BaseRevision: base, Kind: workspacecoord.MergeSnapshot,
		CandidateDigest: "sha256:c2", PatchArtifact: "sha256:p2", Supersedes: first.ID, IdempotencyKey: "merge-2",
	})
	if err != nil || second.Supersedes != first.ID {
		t.Fatalf("enqueue superseding merge: %#v err=%v", second, err)
	}
	replaced, err := store.GetMerge(ctx, first.ID)
	if err != nil || replaced.State != workspacecoord.MergeSuperseded || replaced.SupersededBy != second.ID || replaced.Version != 2 {
		t.Fatalf("superseded merge = %#v err=%v", replaced, err)
	}
	if _, err := store.EnqueueMerge(ctx, workspacecoord.MergeSpec{
		TeamID: root.TeamID, AgentID: child.ID, ViewID: view.ID, BaseRevision: base, Kind: workspacecoord.MergeSnapshot,
		CandidateDigest: "sha256:c3", PatchArtifact: "sha256:p3", Supersedes: first.ID, IdempotencyKey: "merge-3",
	}); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("superseding a closed merge: %v", err)
	}
	listed, err := store.ListMerges(ctx, workspacecoord.MergeQuery{TeamID: root.TeamID, ViewID: view.ID, States: []workspacecoord.MergeState{workspacecoord.MergePending}})
	if err != nil || len(listed) != 1 || listed[0].ID != second.ID {
		t.Fatalf("list by view = %#v err=%v", listed, err)
	}

	plan := workspacecoord.ApplyPlan{Version: 1, MergeID: second.ID, CandidateDigest: "sha256:c2", Digest: "sha256:plan",
		Entries: []workspacecoord.PlanEntry{{Path: "a.txt", Action: workspacecoord.ActionDelete}}}
	if _, err := store.BeginApply(ctx, plan); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("journal accepted a pending merge: %v", err)
	}
	running, err := store.TransitionMerge(ctx, workspacecoord.MergeTransition{MergeID: second.ID, Expected: second.Version, Target: workspacecoord.MergeRunning, PlanDigest: plan.Digest, ActorAgentID: child.ID})
	if err != nil || running.PlanDigest != plan.Digest {
		t.Fatalf("start merge: %#v err=%v", running, err)
	}
	if _, err := store.BeginApply(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkPathApplied(ctx, second.ID, "a.txt"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkPathApplied(ctx, second.ID, "a.txt"); err != nil {
		t.Fatalf("repeated mark: %v", err)
	}
	other := plan
	other.Digest = "sha256:other"
	if _, err := store.BeginApply(ctx, other); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("journal accepted another plan: %v", err)
	}
	record, err := store.BeginApply(ctx, plan)
	if err != nil || !record.Applied["a.txt"] || record.State != workspacecoord.ApplyStarted || len(record.Plan.Entries) != 1 {
		t.Fatalf("resumed record = %#v err=%v", record, err)
	}
	if err := store.FinishApply(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkPathApplied(ctx, second.ID, "b.txt"); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("finished journal accepted progress: %v", err)
	}
	loaded, ok, err := store.LoadApply(ctx, second.ID)
	if err != nil || !ok || loaded.State != workspacecoord.ApplyFinished {
		t.Fatalf("loaded = %#v ok=%v err=%v", loaded, ok, err)
	}
	if _, ok, err := store.LoadApply(ctx, first.ID); err != nil || ok {
		t.Fatalf("unjournaled merge loaded: ok=%v err=%v", ok, err)
	}
}

func TestTaskAwaitingMergeTracksOpenSnapshotMerges(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "awaiting-merge-root"})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := store.CreateChild(ctx, orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker-v1"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, orchestration.TaskSpec{TeamID: root.TeamID, Title: "Snapshot change"})
	if err != nil {
		t.Fatal(err)
	}
	if task, err = store.TransitionTask(ctx, orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskReady}); err != nil {
		t.Fatal(err)
	}
	if task, err = store.ClaimTask(ctx, task.ID, worker.ID, task.Version); err != nil {
		t.Fatal(err)
	}
	if task, err = store.TransitionTask(ctx, orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskInProgress, AssigneeAgentID: worker.ID, ActorAgentID: worker.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionTask(ctx, orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskAwaitingMerge, ActorAgentID: worker.ID}); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("awaiting merge without a merge: %v", err)
	}
	base := "sha256:base"
	view, err := store.CreateView(ctx, workspacecoord.ViewSpec{
		TeamID: root.TeamID, AgentID: worker.ID, TaskID: task.ID, Mode: workspacecoord.ViewSnapshot, WorkspaceRoot: t.TempDir(),
		RootPath: filepath.Join(t.TempDir(), "snap-1", "work"), BaseRevision: base, IdempotencyKey: "view-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	merge, err := store.EnqueueMerge(ctx, workspacecoord.MergeSpec{
		TeamID: root.TeamID, AgentID: worker.ID, TaskID: task.ID, ViewID: view.ID, BaseRevision: base, Kind: workspacecoord.MergeSnapshot,
		CandidateDigest: "sha256:c1", PatchArtifact: "sha256:p1", IdempotencyKey: "merge-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := store.TransitionTask(ctx, orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskAwaitingMerge, ActorAgentID: worker.ID})
	if err != nil || waiting.State != orchestration.TaskAwaitingMerge {
		t.Fatalf("awaiting merge = %#v err=%v", waiting, err)
	}
	if _, err := store.TransitionTask(ctx, orchestration.TaskTransition{TaskID: task.ID, Expected: waiting.Version, Target: orchestration.TaskCompleted, ActorAgentID: worker.ID}); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("completion with an open merge: %v", err)
	}
	if _, err := store.TransitionMerge(ctx, workspacecoord.MergeTransition{MergeID: merge.ID, Expected: merge.Version, Target: workspacecoord.MergeRejected, Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	completed, err := store.TransitionTask(ctx, orchestration.TaskTransition{TaskID: task.ID, Expected: waiting.Version, Target: orchestration.TaskCompleted, ActorAgentID: worker.ID})
	if err != nil || completed.State != orchestration.TaskCompleted {
		t.Fatalf("completion after the merge closed = %#v err=%v", completed, err)
	}
}

// The store must satisfy the journal the commit path expects.
var _ workspacecoord.ApplyJournal = (*OrchestrationStore)(nil)

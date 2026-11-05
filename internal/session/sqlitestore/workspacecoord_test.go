package sqlitestore

import (
	"context"
	"errors"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

func TestWorkspaceViewsAndMergeQueueAreDurableAndVersioned(t *testing.T) {
	database, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: "workspace-view-root"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.CreateChild(context.Background(), orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker-v1"})
	if err != nil {
		t.Fatal(err)
	}
	view, err := store.CreateView(context.Background(), workspacecoord.ViewSpec{
		TeamID: root.TeamID, AgentID: child.ID, Mode: workspacecoord.ViewSharedLease,
		WorkspaceRoot: t.TempDir(), RootPath: t.TempDir(), IdempotencyKey: "view-1",
	})
	if err == nil {
		t.Fatal("shared view accepted a different root path")
	}
	workspaceRoot := t.TempDir()
	view, err = store.CreateView(context.Background(), workspacecoord.ViewSpec{
		TeamID: root.TeamID, AgentID: child.ID, Mode: workspacecoord.ViewSharedLease,
		WorkspaceRoot: workspaceRoot, RootPath: workspaceRoot, IdempotencyKey: "view-1",
	})
	if err != nil || view.State != workspacecoord.ViewActive {
		t.Fatalf("create view failed: %#v err=%v", view, err)
	}
	repeated, err := store.CreateView(context.Background(), workspacecoord.ViewSpec{
		TeamID: root.TeamID, AgentID: child.ID, Mode: workspacecoord.ViewSharedLease,
		WorkspaceRoot: workspaceRoot, RootPath: workspaceRoot, IdempotencyKey: "view-1",
	})
	if err != nil || repeated.ID != view.ID {
		t.Fatalf("view idempotency failed: %#v err=%v", repeated, err)
	}
	if _, err := store.CreateView(context.Background(), workspacecoord.ViewSpec{
		TeamID: root.TeamID, AgentID: child.ID, Mode: workspacecoord.ViewSharedReadonly,
		WorkspaceRoot: workspaceRoot, RootPath: workspaceRoot, IdempotencyKey: "view-1",
	}); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("got %v, want view idempotency conflict", err)
	}
	merge, err := store.EnqueueMerge(context.Background(), workspacecoord.MergeSpec{
		TeamID: root.TeamID, AgentID: child.ID, ViewID: view.ID, BaseRevision: "abc123", PatchArtifact: "sha256:patch", IdempotencyKey: "merge-1",
	})
	if err != nil || merge.State != workspacecoord.MergePending || merge.Version != 1 {
		t.Fatalf("enqueue merge failed: %#v err=%v", merge, err)
	}
	merge, err = store.TransitionMerge(context.Background(), workspacecoord.MergeTransition{MergeID: merge.ID, Expected: merge.Version, Target: workspacecoord.MergeRunning, ActorAgentID: child.ID})
	if err != nil || merge.State != workspacecoord.MergeRunning || merge.Version != 2 {
		t.Fatalf("start merge failed: %#v err=%v", merge, err)
	}
	if _, err := store.TransitionMerge(context.Background(), workspacecoord.MergeTransition{MergeID: merge.ID, Expected: 1, Target: workspacecoord.MergeApplied, ActorAgentID: child.ID}); !errors.Is(err, orchestration.ErrSequenceConflict) {
		t.Fatalf("got %v, want merge version conflict", err)
	}
	merge, err = store.TransitionMerge(context.Background(), workspacecoord.MergeTransition{MergeID: merge.ID, Expected: merge.Version, Target: workspacecoord.MergeApplied, ActorAgentID: child.ID})
	if err != nil || merge.State != workspacecoord.MergeApplied {
		t.Fatalf("apply merge failed: %#v err=%v", merge, err)
	}
	if _, err := store.ReleaseView(context.Background(), view.ID, "merge applied"); err != nil {
		t.Fatal(err)
	}
}

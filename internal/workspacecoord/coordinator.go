package workspacecoord

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

// Coordinator couples durable view state with optional Git worktree
// materialization. Persistence happens before filesystem creation. Failed
// materialization never deletes a destination that may hold recoverable work.
type Coordinator struct {
	Repository Repository
	Worktrees  *WorktreeManager
	Artifacts  ArtifactWriter
	Events     orchestration.EventStore
	// Snapshots materializes snapshot views. Nil disables snapshot mode.
	Snapshots *SnapshotStore
}

// NewCoordinator binds the workspace repository and optional Git worktree manager.
func NewCoordinator(repository Repository, worktrees *WorktreeManager) (*Coordinator, error) {
	if repository == nil {
		return nil, errors.New("workspace coordinator requires a repository")
	}
	return &Coordinator{Repository: repository, Worktrees: worktrees}, nil
}

// CreateSnapshotView materializes the snapshot of request's run, then records
// a view whose root is the snapshot work tree and whose base revision is the
// baseline digest. A retried run reuses both the directory and the view.
func (coordinator *Coordinator) CreateSnapshotView(ctx context.Context, spec ViewSpec, request SnapshotRequest) (View, error) {
	if coordinator == nil || coordinator.Repository == nil {
		return View{}, errors.New("workspace coordinator is unavailable")
	}
	if coordinator.Snapshots == nil {
		return View{}, errors.New("snapshot workspace support is unavailable")
	}
	snapshot, err := coordinator.Snapshots.Materialize(ctx, request)
	if err != nil {
		return View{}, fmt.Errorf("materialize snapshot: %w", err)
	}
	spec.Mode, spec.RootPath, spec.BaseRevision = ViewSnapshot, snapshot.WorkPath, snapshot.Digest
	spec.WorkspaceRoot = coordinator.Snapshots.WorkspaceRoot
	view, err := coordinator.Repository.CreateView(ctx, spec)
	if err != nil {
		return View{}, err
	}
	return view, nil
}

// DiscardSnapshotView deletes a snapshot directory and releases its view.
// Callers decide first that the view holds no undelivered work.
func (coordinator *Coordinator) DiscardSnapshotView(ctx context.Context, viewID orchestration.WorkspaceViewID, reason string) error {
	if coordinator == nil || coordinator.Repository == nil {
		return errors.New("workspace coordinator is unavailable")
	}
	view, err := coordinator.Repository.GetView(ctx, viewID)
	if err != nil {
		return err
	}
	if view.State == ViewReleased {
		return nil
	}
	if coordinator.Snapshots == nil {
		return fmt.Errorf("snapshot retained at %s: snapshot store is unavailable", view.RootPath)
	}
	if err := coordinator.Snapshots.Remove(view); err != nil {
		return err
	}
	_, err = coordinator.Repository.ReleaseView(ctx, view.ID, reason)
	return err
}

// CreateView records the workspace view and materializes a worktree when required by its
// isolation mode.
func (coordinator *Coordinator) CreateView(ctx context.Context, spec ViewSpec) (View, error) {
	if coordinator == nil || coordinator.Repository == nil {
		return View{}, errors.New("workspace coordinator is unavailable")
	}
	view, err := coordinator.Repository.CreateView(ctx, spec)
	if err != nil {
		return View{}, err
	}
	if view.Mode == ViewGitWorktree {
		if coordinator.Worktrees == nil {
			_, _ = coordinator.Repository.ReleaseView(ctx, view.ID, "worktree manager unavailable")
			return View{}, errors.New("git worktree support is unavailable")
		}
		if err := coordinator.Worktrees.Create(ctx, view); err != nil {
			// Creation can fail after materializing files, or encounter a view
			// from a previous attempt. Retain its durable identity for recovery;
			// neither case authorizes deleting the directory.
			if _, inspectErr := os.Lstat(view.RootPath); errors.Is(inspectErr, os.ErrNotExist) {
				_, _ = coordinator.Repository.ReleaseView(ctx, view.ID, "worktree creation failed before materialization")
			}
			return View{}, fmt.Errorf("materialize workspace view: %w", err)
		}
	}
	return view, nil
}

func (coordinator *Coordinator) ReleaseView(ctx context.Context, viewID orchestration.WorkspaceViewID, reason string) error {
	if coordinator == nil || coordinator.Repository == nil {
		return errors.New("workspace coordinator is unavailable")
	}
	view, err := coordinator.Repository.GetView(ctx, viewID)
	if err != nil {
		return err
	}
	if view.Mode == ViewGitWorktree && view.State == ViewActive && coordinator.Worktrees != nil {
		if err := coordinator.Worktrees.Remove(ctx, view); err != nil {
			return fmt.Errorf("remove workspace worktree: %w", err)
		}
	}
	_, err = coordinator.Repository.ReleaseView(ctx, view.ID, reason)
	return err
}

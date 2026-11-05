package workspacecoord

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

type ArtifactWriter interface {
	Put(context.Context, []byte) (string, error)
}

// FinalizeView ends a worker's use of its view without treating task completion
// as permission to merge or discard its output. Git views stay addressable for
// review/recovery, including ignored files that are intentionally not a patch.
// Shared views have no private output to preserve and can be released directly.
func (coordinator *Coordinator) FinalizeView(ctx context.Context, viewID orchestration.WorkspaceViewID, reason string) error {
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
	if view.Mode == ViewSnapshot {
		return coordinator.finalizeSnapshotView(ctx, view, reason)
	}
	if view.Mode != ViewGitWorktree {
		return coordinator.ReleaseView(ctx, viewID, reason)
	}
	if coordinator.Artifacts == nil || coordinator.Events == nil {
		return fmt.Errorf("worktree retained at %s: preservation storage is unavailable", view.RootPath)
	}
	patch, err := coordinator.Worktrees.Diff(ctx, view)
	if err != nil {
		return fmt.Errorf("worktree retained at %s: capture patch: %w", view.RootPath, err)
	}
	reference, err := coordinator.Artifacts.Put(ctx, patch)
	if err != nil {
		return fmt.Errorf("worktree retained at %s: store patch: %w", view.RootPath, err)
	}
	err = coordinator.recordPreservation(ctx, view, orchestration.PendingEvent{
		Type: orchestration.EventWorkspaceViewPreserved, ActorAgentID: view.AgentID,
		CorrelationID: view.CorrelationID, IdempotencyKey: "preserve:" + string(view.ID) + ":" + reference,
		Payload: orchestration.MustJSONPayload(map[string]any{
			"view_id": view.ID, "agent_id": view.AgentID, "task_id": view.TaskID,
			"root_path": view.RootPath, "base_revision": view.BaseRevision,
			"patch_artifact": reference, "patch_bytes": len(patch), "reason": reason,
		}),
	})
	if err != nil {
		return fmt.Errorf("worktree retained at %s: record patch reference: %w", view.RootPath, err)
	}
	return nil
}

// EventStore idempotency keys are audit fields, not unique constraints. Use
// stream CAS so repeated/concurrent finalization cannot publish duplicate
// references. A failed CAS never permits worktree removal.
func (coordinator *Coordinator) recordPreservation(ctx context.Context, view View, pending orchestration.PendingEvent) error {
	stream := orchestration.Stream{Type: orchestration.StreamWorkspace, ID: string(view.TeamID), TeamID: view.TeamID}
	for range 5 {
		var after uint64
		for {
			events, err := coordinator.Events.LoadStream(ctx, stream.Type, stream.ID, after, 256)
			if err != nil {
				return err
			}
			if len(events) == 0 {
				break
			}
			for _, event := range events {
				if event.Type == pending.Type && event.IdempotencyKey == pending.IdempotencyKey {
					return nil
				}
				after = event.StreamSequence
			}
		}
		_, err := coordinator.Events.Append(ctx, stream, after, pending)
		if !errors.Is(err, orchestration.ErrSequenceConflict) {
			return err
		}
	}
	return orchestration.ErrSequenceConflict
}

// finalizeSnapshotView discards a snapshot that holds no undelivered work:
// one without changes, or one whose merge was applied. A snapshot with an
// open merge stays for that merge. Any other snapshot with changes stays on
// disk and its location is recorded for review.
func (coordinator *Coordinator) finalizeSnapshotView(ctx context.Context, view View, reason string) error {
	if coordinator.Snapshots == nil {
		return fmt.Errorf("snapshot retained at %s: snapshot store is unavailable", view.RootPath)
	}
	snapshot, err := coordinator.Snapshots.ForView(view)
	if errors.Is(err, os.ErrNotExist) {
		_, err = coordinator.Repository.ReleaseView(ctx, view.ID, reason)
		return err
	}
	if err != nil {
		return fmt.Errorf("snapshot retained at %s: %w", view.RootPath, err)
	}
	work, err := WorkManifest(ctx, snapshot)
	if err != nil {
		return fmt.Errorf("snapshot retained at %s: %w", view.RootPath, err)
	}
	if work.Digest() == snapshot.Digest {
		return coordinator.DiscardSnapshotView(ctx, view.ID, reason)
	}
	merges, err := coordinator.Repository.ListMerges(ctx, MergeQuery{TeamID: view.TeamID, ViewID: view.ID})
	if err != nil {
		return fmt.Errorf("snapshot retained at %s: list merges: %w", view.RootPath, err)
	}
	applied := false
	for _, merge := range merges {
		switch merge.State {
		case MergeApplied:
			applied = true
		case MergePending, MergeRunning, MergeConflicted:
			return nil
		}
	}
	if applied {
		return coordinator.DiscardSnapshotView(ctx, view.ID, reason)
	}
	if coordinator.Events == nil {
		return fmt.Errorf("snapshot retained at %s: preservation storage is unavailable", view.RootPath)
	}
	err = coordinator.recordPreservation(ctx, view, orchestration.PendingEvent{
		Type: orchestration.EventWorkspaceViewPreserved, ActorAgentID: view.AgentID,
		CorrelationID: view.CorrelationID, IdempotencyKey: "preserve:" + string(view.ID) + ":" + work.Digest(),
		Payload: orchestration.MustJSONPayload(map[string]any{
			"view_id": view.ID, "agent_id": view.AgentID, "task_id": view.TaskID,
			"root_path": view.RootPath, "base_revision": view.BaseRevision,
			"work_digest": work.Digest(), "reason": reason,
		}),
	})
	if err != nil {
		return fmt.Errorf("snapshot retained at %s: record snapshot location: %w", view.RootPath, err)
	}
	return nil
}

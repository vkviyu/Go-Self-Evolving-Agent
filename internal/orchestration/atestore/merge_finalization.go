package atestore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

const mergeFinalizationKind = "merge-finalization"

func mergeSameFinalization(a, b workspacecoord.MergeFinalization) bool {
	return a.BindingDigest == b.BindingDigest && a.WorkspaceDigest == b.WorkspaceDigest && a.VerificationDigest == b.VerificationDigest
}

func (store *Store) BeginMergeFinalization(ctx context.Context, receipt workspacecoord.MergeFinalization) (workspacecoord.MergeFinalization, error) {
	if receipt.State != workspacecoord.FinalizationPrepared || receipt.BindingDigest == "" || receipt.Settled {
		return workspacecoord.MergeFinalization{}, fmt.Errorf("%w: invalid initial merge finalization", orchestration.ErrInvalidEvent)
	}
	if err := receipt.MergeID.Validate(); err != nil {
		return workspacecoord.MergeFinalization{}, err
	}
	var result workspacecoord.MergeFinalization
	err := store.authority.Update(workspaceContext(ctx), func(tx *ateauthority.Tx) error {
		existing, err := getRecord[workspacecoord.MergeFinalization](tx, mergeFinalizationKind, string(receipt.MergeID))
		if err == nil {
			if !mergeSameFinalization(existing, receipt) {
				return fmt.Errorf("%w: merge finalization identity differs", orchestration.ErrIdempotencyConflict)
			}
			result = existing
			return nil
		}
		if !errors.Is(err, orchestration.ErrNotFound) {
			return err
		}
		// SQL's foreign key also requires an existing merge. Preparation may
		// precede recording MergeApplied, so it cannot require that state.
		if _, err := getRecord[workspacecoord.MergeRequest](tx, mergeRecordKind, string(receipt.MergeID)); err != nil {
			return err
		}
		if err := createRecord(tx, mergeFinalizationKind, string(receipt.MergeID), receipt); err != nil {
			return err
		}
		result = receipt
		return nil
	})
	if err != nil {
		return workspacecoord.MergeFinalization{}, err
	}
	return result, nil
}

func (store *Store) LoadMergeFinalization(ctx context.Context, id orchestration.MergeID) (workspacecoord.MergeFinalization, bool, error) {
	var result workspacecoord.MergeFinalization
	var found bool
	err := store.authority.View(workspaceContext(ctx), func(tx *ateauthority.Tx) error {
		var err error
		result, err = getRecord[workspacecoord.MergeFinalization](tx, mergeFinalizationKind, string(id))
		if errors.Is(err, orchestration.ErrNotFound) {
			return nil
		}
		found = err == nil
		return err
	})
	if err != nil {
		return workspacecoord.MergeFinalization{}, false, err
	}
	return result, found, nil
}

func (store *Store) TransitionMergeFinalization(ctx context.Context, receipt workspacecoord.MergeFinalization, previous workspacecoord.FinalizationState) (workspacecoord.MergeFinalization, error) {
	valid := previous == workspacecoord.FinalizationPrepared && (receipt.State == workspacecoord.FinalizationDispatched || receipt.State == workspacecoord.FinalizationCompleted) ||
		previous == workspacecoord.FinalizationDispatched && (receipt.State == workspacecoord.FinalizationCompleted || receipt.State == workspacecoord.FinalizationUnknown)
	if !valid || receipt.Settled || receipt.BindingDigest == "" {
		return workspacecoord.MergeFinalization{}, fmt.Errorf("%w: invalid merge finalization transition", orchestration.ErrInvalidEvent)
	}
	if receipt.State != workspacecoord.FinalizationDispatched && receipt.TargetState != orchestration.TaskCompleted && receipt.TargetState != orchestration.TaskFailed {
		return workspacecoord.MergeFinalization{}, fmt.Errorf("%w: invalid finalization outcome", orchestration.ErrInvalidEvent)
	}
	var result workspacecoord.MergeFinalization
	err := store.authority.Update(workspaceContext(ctx), func(tx *ateauthority.Tx) error {
		existing, err := getRecord[workspacecoord.MergeFinalization](tx, mergeFinalizationKind, string(receipt.MergeID))
		if err != nil {
			return err
		}
		if !mergeSameFinalization(existing, receipt) || existing.Settled {
			return fmt.Errorf("%w: merge finalization identity differs", orchestration.ErrIdempotencyConflict)
		}
		if existing.State == receipt.State && existing.TargetState == receipt.TargetState && existing.Reason == receipt.Reason {
			result = existing
			return nil
		}
		if existing.State != previous {
			return fmt.Errorf("%w: merge finalization state is %s", orchestration.ErrSequenceConflict, existing.State)
		}
		if err := putRecord(tx, mergeFinalizationKind, string(receipt.MergeID), receipt); err != nil {
			return err
		}
		result = receipt
		return nil
	})
	if err != nil {
		return workspacecoord.MergeFinalization{}, err
	}
	return result, nil
}

// SettleMergeFinalization commits delivery facts and the receipt together.
// Filesystem disposal and verification belong to the caller, never this
// retriable transaction. Previously reconciled tasks/views remain idempotent.
func (store *Store) SettleMergeFinalization(ctx context.Context, id orchestration.MergeID, binding string) error {
	now := time.Now().UTC()
	return store.authority.Update(workspaceContext(ctx), func(tx *ateauthority.Tx) error {
		receipt, err := getRecord[workspacecoord.MergeFinalization](tx, mergeFinalizationKind, string(id))
		if errors.Is(err, orchestration.ErrNotFound) {
			return fmt.Errorf("%w: merge finalization was not completed", orchestration.ErrSequenceConflict)
		}
		if err != nil {
			return err
		}
		if receipt.BindingDigest != binding || receipt.State != workspacecoord.FinalizationCompleted && receipt.State != workspacecoord.FinalizationUnknown {
			return fmt.Errorf("%w: merge finalization was not completed", orchestration.ErrSequenceConflict)
		}
		if receipt.Settled {
			return nil
		}
		if receipt.TargetState != orchestration.TaskCompleted && receipt.TargetState != orchestration.TaskFailed {
			return fmt.Errorf("%w: invalid finalization outcome", orchestration.ErrInvalidEvent)
		}
		applied, err := getRecord[workspacecoord.MergeRequest](tx, mergeRecordKind, string(id))
		if err != nil {
			return err
		}
		if applied.State != workspacecoord.MergeApplied {
			return fmt.Errorf("%w: merge is not applied", orchestration.ErrSequenceConflict)
		}
		if workspacecoord.MergeBindingDigest(applied) != binding {
			return fmt.Errorf("%w: merge finalization binding differs from applied merge", orchestration.ErrIdempotencyConflict)
		}
		seen := make(map[orchestration.MergeID]bool)
		for delivery := applied; ; {
			if seen[delivery.ID] {
				return fmt.Errorf("%w: cyclic superseded merge chain", orchestration.ErrInvalidEvent)
			}
			seen[delivery.ID] = true
			if err := mergeSettleDeliveryTx(tx, applied, delivery, receipt, now); err != nil {
				return err
			}
			if delivery.Supersedes == "" {
				break
			}
			previous, err := getRecord[workspacecoord.MergeRequest](tx, mergeRecordKind, string(delivery.Supersedes))
			if err != nil {
				return err
			}
			if previous.TeamID != applied.TeamID || previous.State != workspacecoord.MergeSuperseded || previous.SupersededBy != delivery.ID {
				return fmt.Errorf("%w: invalid superseded delivery relation", orchestration.ErrInvalidEvent)
			}
			delivery = previous
		}
		receipt.Settled = true
		return putRecord(tx, mergeFinalizationKind, string(id), receipt)
	})
}

func mergeSettleDeliveryTx(tx *ateauthority.Tx, applied, delivery workspacecoord.MergeRequest, receipt workspacecoord.MergeFinalization, now time.Time) error {
	if delivery.TaskID != "" {
		task, err := getRecord[orchestration.TaskSnapshot](tx, "task", string(delivery.TaskID))
		if err != nil {
			return err
		}
		if task.TeamID != applied.TeamID {
			return fmt.Errorf("%w: delivered task belongs to another team", orchestration.ErrInvalidEvent)
		}
		if task.State == orchestration.TaskAwaitingMerge {
			if _, err := transitionTaskTx(tx, orchestration.TaskTransition{
				TaskID: task.ID, Expected: task.Version, Target: receipt.TargetState,
				ActorAgentID: applied.AgentID, Reason: receipt.Reason,
			}, now); err != nil {
				return err
			}
		}
	}
	view, err := getRecord[workspacecoord.View](tx, workspaceViewKind, string(delivery.ViewID))
	if err != nil {
		return err
	}
	if view.TeamID != applied.TeamID || view.AgentID != delivery.AgentID || view.TaskID != "" && view.TaskID != delivery.TaskID {
		return fmt.Errorf("%w: delivered workspace view relation differs", orchestration.ErrInvalidEvent)
	}
	if delivery.Kind == workspacecoord.MergeSnapshot && receipt.TargetState != orchestration.TaskCompleted {
		return nil // Preserve failed snapshot candidates for investigation.
	}
	if view.Mode == workspacecoord.ViewGitWorktree {
		// Coordinator.FinalizeView preserves a worktree and its patch; it
		// deliberately leaves the view active. Settlement must not hide it
		// from recovery or authorize removal without that filesystem workflow.
		return nil
	}
	_, err = workspaceReleaseViewTx(tx, view.ID, receipt.Reason, now)
	return err
}

func (store *Store) PendingMergeFinalizations(ctx context.Context, team orchestration.TeamID, limit int) ([]workspacecoord.MergeRequest, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var result []workspacecoord.MergeRequest
	err := store.authority.View(workspaceContext(ctx), func(tx *ateauthority.Tx) error {
		merges, err := mergeListTx(tx, workspacecoord.MergeQuery{TeamID: team, States: []workspacecoord.MergeState{workspacecoord.MergeApplied}})
		if err != nil {
			return err
		}
		for _, merge := range merges {
			receipt, err := getRecord[workspacecoord.MergeFinalization](tx, mergeFinalizationKind, string(merge.ID))
			if err != nil && !errors.Is(err, orchestration.ErrNotFound) {
				return err
			}
			if err != nil || !receipt.Settled {
				result = append(result, merge)
				if len(result) == limit {
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

var _ workspacecoord.FinalizationStore = (*Store)(nil)

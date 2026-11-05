package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

func (store *OrchestrationStore) BeginMergeFinalization(ctx context.Context, receipt workspacecoord.MergeFinalization) (workspacecoord.MergeFinalization, error) {
	if receipt.State != workspacecoord.FinalizationPrepared || receipt.BindingDigest == "" || receipt.Settled {
		return workspacecoord.MergeFinalization{}, fmt.Errorf("%w: invalid initial merge finalization", orchestration.ErrInvalidEvent)
	}
	if err := receipt.MergeID.Validate(); err != nil {
		return workspacecoord.MergeFinalization{}, err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return workspacecoord.MergeFinalization{}, err
	}
	defer func() { _ = tx.Rollback() }()
	existing, found, err := loadMergeFinalization(ctx, tx, receipt.MergeID)
	if err != nil {
		return workspacecoord.MergeFinalization{}, err
	}
	if found {
		if existing.BindingDigest != receipt.BindingDigest || existing.WorkspaceDigest != receipt.WorkspaceDigest || existing.VerificationDigest != receipt.VerificationDigest {
			return workspacecoord.MergeFinalization{}, fmt.Errorf("%w: merge finalization identity differs", orchestration.ErrIdempotencyConflict)
		}
		return existing, tx.Commit()
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO merge_finalizations(merge_id,binding_digest,workspace_digest,verification_digest,state,target_state,reason,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, string(receipt.MergeID), receipt.BindingDigest, receipt.WorkspaceDigest, receipt.VerificationDigest, string(receipt.State), string(receipt.TargetState), receipt.Reason, now, now); err != nil {
		return workspacecoord.MergeFinalization{}, fmt.Errorf("begin merge finalization: %w", err)
	}
	return receipt, tx.Commit()
}

func (store *OrchestrationStore) LoadMergeFinalization(ctx context.Context, id orchestration.MergeID) (workspacecoord.MergeFinalization, bool, error) {
	return loadMergeFinalization(ctx, store.db, id)
}

func loadMergeFinalization(ctx context.Context, query rowQuery, id orchestration.MergeID) (workspacecoord.MergeFinalization, bool, error) {
	receipt := workspacecoord.MergeFinalization{MergeID: id}
	err := query.QueryRowContext(ctx, `SELECT binding_digest,workspace_digest,verification_digest,state,target_state,reason,settled FROM merge_finalizations WHERE merge_id=?`, string(id)).Scan(&receipt.BindingDigest, &receipt.WorkspaceDigest, &receipt.VerificationDigest, &receipt.State, &receipt.TargetState, &receipt.Reason, &receipt.Settled)
	if errors.Is(err, sql.ErrNoRows) {
		return workspacecoord.MergeFinalization{}, false, nil
	}
	return receipt, err == nil, err
}

func (store *OrchestrationStore) TransitionMergeFinalization(ctx context.Context, receipt workspacecoord.MergeFinalization, previous workspacecoord.FinalizationState) (workspacecoord.MergeFinalization, error) {
	valid := previous == workspacecoord.FinalizationPrepared && (receipt.State == workspacecoord.FinalizationDispatched || receipt.State == workspacecoord.FinalizationCompleted) ||
		previous == workspacecoord.FinalizationDispatched && (receipt.State == workspacecoord.FinalizationCompleted || receipt.State == workspacecoord.FinalizationUnknown)
	if !valid || receipt.Settled || receipt.BindingDigest == "" {
		return workspacecoord.MergeFinalization{}, fmt.Errorf("%w: invalid merge finalization transition", orchestration.ErrInvalidEvent)
	}
	if receipt.State != workspacecoord.FinalizationDispatched && receipt.TargetState != orchestration.TaskCompleted && receipt.TargetState != orchestration.TaskFailed {
		return workspacecoord.MergeFinalization{}, fmt.Errorf("%w: invalid finalization outcome", orchestration.ErrInvalidEvent)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return workspacecoord.MergeFinalization{}, err
	}
	defer func() { _ = tx.Rollback() }()
	existing, found, err := loadMergeFinalization(ctx, tx, receipt.MergeID)
	if err != nil {
		return workspacecoord.MergeFinalization{}, err
	}
	if !found {
		return workspacecoord.MergeFinalization{}, fmt.Errorf("%w: merge finalization", orchestration.ErrNotFound)
	}
	if existing.BindingDigest != receipt.BindingDigest || existing.WorkspaceDigest != receipt.WorkspaceDigest || existing.VerificationDigest != receipt.VerificationDigest || existing.Settled {
		return workspacecoord.MergeFinalization{}, fmt.Errorf("%w: merge finalization identity differs", orchestration.ErrIdempotencyConflict)
	}
	if existing.State == receipt.State && existing.TargetState == receipt.TargetState && existing.Reason == receipt.Reason {
		return existing, tx.Commit()
	}
	if existing.State != previous {
		return workspacecoord.MergeFinalization{}, fmt.Errorf("%w: merge finalization state is %s", orchestration.ErrSequenceConflict, existing.State)
	}
	if err := execCAS(ctx, tx, orchestration.ErrSequenceConflict, `UPDATE merge_finalizations SET state=?,target_state=?,reason=?,updated_at=? WHERE merge_id=? AND binding_digest=? AND state=? AND settled=0`, string(receipt.State), string(receipt.TargetState), receipt.Reason, time.Now().UTC().Format(time.RFC3339Nano), string(receipt.MergeID), receipt.BindingDigest, string(previous)); err != nil {
		return workspacecoord.MergeFinalization{}, err
	}
	return receipt, tx.Commit()
}

func (store *OrchestrationStore) SettleMergeFinalization(ctx context.Context, id orchestration.MergeID, binding string) error {
	result, err := store.db.ExecContext(ctx, `UPDATE merge_finalizations SET settled=1,updated_at=? WHERE merge_id=? AND binding_digest=? AND state IN ('completed','unknown')`, time.Now().UTC().Format(time.RFC3339Nano), string(id), binding)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("%w: merge finalization was not completed", orchestration.ErrSequenceConflict)
	}
	return nil
}

func (store *OrchestrationStore) PendingMergeFinalizations(ctx context.Context, team orchestration.TeamID, limit int) ([]workspacecoord.MergeRequest, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := store.db.QueryContext(ctx, `SELECT m.merge_id FROM merge_queue m LEFT JOIN merge_finalizations f ON f.merge_id=m.merge_id WHERE m.team_id=? AND m.state='applied' AND (f.merge_id IS NULL OR f.settled=0) ORDER BY m.created_at,m.merge_id LIMIT ?`, string(team), limit)
	if err != nil {
		return nil, err
	}
	var ids []orchestration.MergeID
	for rows.Next() {
		var id orchestration.MergeID
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var merges []workspacecoord.MergeRequest
	for _, id := range ids {
		merge, err := store.GetMerge(ctx, id)
		if err != nil {
			return nil, err
		}
		merges = append(merges, merge)
	}
	return merges, nil
}

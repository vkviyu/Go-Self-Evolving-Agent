package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

// BeginApply journals plan before any main-workspace path changes. A second
// call with the same digest returns the stored record and its progress.
func (store *OrchestrationStore) BeginApply(ctx context.Context, plan workspacecoord.ApplyPlan) (workspacecoord.ApplyRecord, error) {
	if store == nil || store.db == nil {
		return workspacecoord.ApplyRecord{}, errors.New("orchestration store is unavailable")
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		return workspacecoord.ApplyRecord{}, fmt.Errorf("encode apply plan: %w", err)
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return workspacecoord.ApplyRecord{}, err
	}
	defer func() { _ = tx.Rollback() }()
	record, found, err := applyRecordTx(ctx, tx, plan.MergeID)
	if err != nil {
		return workspacecoord.ApplyRecord{}, err
	}
	if found {
		if record.Plan.Digest != plan.Digest {
			return workspacecoord.ApplyRecord{}, fmt.Errorf("%w: merge %s already journals another plan", orchestration.ErrInvalidEvent, plan.MergeID)
		}
		return record, tx.Commit()
	}
	merge, err := mergeByID(ctx, tx, plan.MergeID)
	if errors.Is(err, sql.ErrNoRows) {
		return workspacecoord.ApplyRecord{}, fmt.Errorf("%w: merge %s", orchestration.ErrNotFound, plan.MergeID)
	}
	if err != nil {
		return workspacecoord.ApplyRecord{}, err
	}
	if merge.State != workspacecoord.MergeRunning {
		return workspacecoord.ApplyRecord{}, fmt.Errorf("%w: merge %s is %s, not running", orchestration.ErrInvalidEvent, plan.MergeID, merge.State)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO merge_apply_journal(merge_id, plan_digest, plan_json, state, created_at, updated_at) VALUES(?, ?, ?, ?, ?, ?)`, string(plan.MergeID), plan.Digest, encoded, string(workspacecoord.ApplyStarted), now, now); err != nil {
		return workspacecoord.ApplyRecord{}, fmt.Errorf("insert apply journal: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return workspacecoord.ApplyRecord{}, err
	}
	return workspacecoord.ApplyRecord{MergeID: plan.MergeID, Plan: plan, State: workspacecoord.ApplyStarted, Applied: map[string]bool{}}, nil
}

// MarkPathApplied records that one path reached its postimage.
func (store *OrchestrationStore) MarkPathApplied(ctx context.Context, mergeID orchestration.MergeID, path string) error {
	result, err := store.db.ExecContext(ctx, `INSERT INTO merge_apply_progress(merge_id, path, applied_at) SELECT merge_id, ?, ? FROM merge_apply_journal WHERE merge_id = ? AND state = ? ON CONFLICT(merge_id, path) DO NOTHING`, path, time.Now().UTC().Format(time.RFC3339Nano), string(mergeID), string(workspacecoord.ApplyStarted))
	if err != nil {
		return fmt.Errorf("record applied path: %w", err)
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return applyJournalOpen(ctx, store.db, mergeID)
	}
	return nil
}

// FinishApply marks the journaled commit complete.
func (store *OrchestrationStore) FinishApply(ctx context.Context, mergeID orchestration.MergeID) error {
	result, err := store.db.ExecContext(ctx, `UPDATE merge_apply_journal SET state = ?, updated_at = ? WHERE merge_id = ?`, string(workspacecoord.ApplyFinished), time.Now().UTC().Format(time.RFC3339Nano), string(mergeID))
	if err != nil {
		return fmt.Errorf("finish apply journal: %w", err)
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return fmt.Errorf("%w: merge %s has no apply journal", orchestration.ErrNotFound, mergeID)
	}
	return nil
}

// LoadApply returns the journal of a merge, if one exists.
func (store *OrchestrationStore) LoadApply(ctx context.Context, mergeID orchestration.MergeID) (workspacecoord.ApplyRecord, bool, error) {
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return workspacecoord.ApplyRecord{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	return applyRecordTx(ctx, tx, mergeID)
}

// applyJournalOpen explains why a progress insert changed nothing: either
// the path was already recorded, or the journal is missing or finished.
func applyJournalOpen(ctx context.Context, query rowQuery, mergeID orchestration.MergeID) error {
	var state string
	err := query.QueryRowContext(ctx, `SELECT state FROM merge_apply_journal WHERE merge_id = ?`, string(mergeID)).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: merge %s has no apply journal", orchestration.ErrNotFound, mergeID)
	}
	if err != nil {
		return err
	}
	if workspacecoord.ApplyState(state) != workspacecoord.ApplyStarted {
		return fmt.Errorf("%w: apply journal of merge %s is %s", orchestration.ErrInvalidEvent, mergeID, state)
	}
	return nil
}

// applyRecordTx reads a journal and its applied paths.
func applyRecordTx(ctx context.Context, tx *sql.Tx, mergeID orchestration.MergeID) (workspacecoord.ApplyRecord, bool, error) {
	var encoded []byte
	var state string
	err := tx.QueryRowContext(ctx, `SELECT plan_json, state FROM merge_apply_journal WHERE merge_id = ?`, string(mergeID)).Scan(&encoded, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return workspacecoord.ApplyRecord{}, false, nil
	}
	if err != nil {
		return workspacecoord.ApplyRecord{}, false, err
	}
	record := workspacecoord.ApplyRecord{MergeID: mergeID, State: workspacecoord.ApplyState(state), Applied: map[string]bool{}}
	if err := json.Unmarshal(encoded, &record.Plan); err != nil {
		return workspacecoord.ApplyRecord{}, false, fmt.Errorf("decode apply plan: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT path FROM merge_apply_progress WHERE merge_id = ?`, string(mergeID))
	if err != nil {
		return workspacecoord.ApplyRecord{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return workspacecoord.ApplyRecord{}, false, err
		}
		record.Applied[path] = true
	}
	return record, true, rows.Err()
}

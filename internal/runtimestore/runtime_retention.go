package runtimestore

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/retention"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

// RuntimeRetentionReferences supplies candidates, never lifecycle proof.
// Flow membership is resolved in ATE, not projected into the runtime database.
func (database *Database) RuntimeRetentionReferences(ctx context.Context, id session.ID) ([]retention.Consumer, error) {
	rows, err := database.db.QueryContext(ctx, `SELECT kind,scope,consumer_id,snapshot_id FROM runtime_snapshot_refs WHERE (kind='turn' AND scope=?) OR kind='flow' ORDER BY kind,scope,consumer_id`, string(id))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []retention.Consumer{}
	for rows.Next() {
		var c retention.Consumer
		if err := rows.Scan(&c.Reference.Kind, &c.Reference.Scope, &c.Reference.ID, &c.SnapshotID); err != nil {
			return nil, err
		}
		if err := c.Reference.Validate(); err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

// CleanupRetention accepts a completed receipt from the trusted authority
// adapter. It records only resource release metadata; business receipts and
// closure facts remain in ATE. The transaction is local and independently
// retryable after native closure. Manifests, history and plugin evidence remain.
func (database *Database) CleanupRetention(ctx context.Context, receipt retention.Receipt) (int, error) {
	if receipt.Plan.ID == "" || receipt.Plan.SessionID == "" || receipt.ClosedAt.IsZero() || receipt.EvidenceDeleted || len(receipt.Plan.Blockers) != 0 || len(receipt.Plan.Consumers) == 0 {
		return 0, errors.New("invalid authoritative retention receipt")
	}
	tx, err := database.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	count := 0
	seen := make(map[tool.SnapshotReference]bool)
	for _, c := range receipt.Plan.Consumers {
		ref := c.Reference
		if err := ref.Validate(); err != nil {
			return 0, err
		}
		if seen[ref] || (ref.Kind == "turn" && ref.Scope != string(receipt.Plan.SessionID)) {
			return 0, errors.New("invalid retention consumer")
		}
		seen[ref] = true
		var snapshot, reason string
		var released sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT snapshot_id,released_at,release_reason FROM runtime_snapshot_refs WHERE kind=? AND scope=? AND consumer_id=?`, ref.Kind, ref.Scope, ref.ID).Scan(&snapshot, &released, &reason)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		} // Resource belongs to another runtime.
		if err != nil {
			return 0, err
		}
		if c.SnapshotID != "" && snapshot != c.SnapshotID {
			return 0, tool.ErrSnapshotReferenceConflict
		}
		expectedReason := "archived by retention plan " + receipt.Plan.ID
		if released.Valid && reason != expectedReason {
			return 0, errors.New("runtime reference released by another operation")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE runtime_snapshot_refs SET released_at=?,release_reason=? WHERE kind=? AND scope=? AND consumer_id=? AND released_at IS NULL`,
			receipt.ClosedAt.Format(time.RFC3339Nano), expectedReason, ref.Kind, ref.Scope, ref.ID); err != nil {
			return 0, err
		}
		count++
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

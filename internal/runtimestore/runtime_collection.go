package runtimestore

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/retention"
)

// RuntimePluginCollectionPlan audits only this runtime's artifact resources
// and immutable snapshot references. It never queries legacy business tables.
func (database *Database) RuntimePluginCollectionPlan(ctx context.Context, name, revision string) (retention.CollectionPlan, error) {
	tx, err := database.db.BeginTx(ctx, nil)
	if err != nil {
		return retention.CollectionPlan{}, err
	}
	defer tx.Rollback()
	return planPluginCollectionResources(ctx, tx, name, revision)
}

// CleanupPluginCollection requires an authorized native receipt from the
// trusted host. The local transaction retains artifact identity metadata for
// replay and anti-resurrection; it does not persist a business receipt.
func (database *Database) CleanupPluginCollection(ctx context.Context, receipt retention.CollectionReceipt) (retention.CollectionReceipt, error) {
	plan := receipt.Plan
	if plan.ID == "" || plan.Tool == "" || plan.Revision == "" || receipt.CollectedAt.IsZero() || len(plan.Blockers) != 0 {
		return receipt, errors.New("invalid authoritative plugin collection receipt")
	}
	tx, err := database.db.BeginTx(ctx, nil)
	if err != nil {
		return receipt, err
	}
	defer tx.Rollback()
	var previous, content, collected string
	err = tx.QueryRowContext(ctx, `SELECT plan_id,content_sha,collected_at FROM plugin_revision_tombstones WHERE tool_name=? AND revision=?`, plan.Tool, plan.Revision).Scan(&previous, &content, &collected)
	if err == nil {
		if previous != plan.ID || content != plan.ContentSHA256 || collected != receipt.CollectedAt.Format(time.RFC3339Nano) {
			return receipt, retention.ErrPlanChanged
		}
		var revisions int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM plugin_revisions WHERE tool_name=? AND revision=?`, plan.Tool, plan.Revision).Scan(&revisions); err != nil {
			return receipt, err
		}
		if revisions != 0 {
			return receipt, errors.New("collected plugin revision was resurrected")
		}
		receipt.BlobDeleted = plan.ReclaimableBytes > 0
		return receipt, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return receipt, err
	}
	current, err := planPluginCollectionResources(ctx, tx, plan.Tool, plan.Revision)
	if err != nil {
		return receipt, err
	}
	if current.Digest() != plan.Digest() {
		return receipt, retention.ErrPlanChanged
	}
	if len(current.Blockers) != 0 {
		return receipt, retention.ErrBlocked
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO plugin_revision_tombstones(tool_name,revision,plugin_id,record,record_sha,content_sha,plan_id,collected_at) SELECT tool_name,revision,plugin_id,record,record_sha,content_sha,?,? FROM plugin_revisions WHERE tool_name=? AND revision=?`,
		plan.ID, receipt.CollectedAt.Format(time.RFC3339Nano), plan.Tool, plan.Revision); err != nil {
		return receipt, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_revisions WHERE tool_name=? AND revision=?`, plan.Tool, plan.Revision); err != nil {
		return receipt, err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM plugin_blobs WHERE content_sha=? AND NOT EXISTS(SELECT 1 FROM plugin_revisions WHERE content_sha=?)`, plan.ContentSHA256, plan.ContentSHA256)
	if err != nil {
		return receipt, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return receipt, err
	}
	receipt.BlobDeleted = count > 0
	if err := tx.Commit(); err != nil {
		return retention.CollectionReceipt{}, err
	}
	return receipt, nil
}

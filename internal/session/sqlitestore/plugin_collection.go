package sqlitestore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/retention"
)

func (database *Database) PluginCollectionPlan(ctx context.Context, name, revision string) (retention.CollectionPlan, error) {
	tx, err := database.db.BeginTx(ctx, nil)
	if err != nil {
		return retention.CollectionPlan{}, err
	}
	defer tx.Rollback()
	return planPluginCollection(ctx, tx, name, revision)
}

// CollectPlugin requires offline exclusive ownership, a freshly recomputed
// reviewed plan and explicit destructive-action confirmation. Deletion and its
// immutable identity tombstone/audit receipt commit in one SQLite transaction.
func (database *Database) CollectPlugin(ctx context.Context, name, revision, planID string, confirmed bool) (retention.CollectionReceipt, error) {
	var receipt retention.CollectionReceipt
	if !confirmed || planID == "" || name == "" || revision == "" {
		return receipt, errors.New("plugin collection requires exact revision, plan ID and explicit confirmation")
	}
	tx, err := database.db.BeginTx(ctx, nil)
	if err != nil {
		return receipt, err
	}
	defer tx.Rollback()
	found, err := loadMaintenanceReceipt(ctx, tx, planID, "plugin_collection", &receipt)
	if err != nil {
		return receipt, err
	}
	if found {
		if receipt.Plan.Tool != name || receipt.Plan.Revision != revision {
			return retention.CollectionReceipt{}, retention.ErrPlanChanged
		}
		return receipt, nil
	}
	plan, err := planPluginCollection(ctx, tx, name, revision)
	if err != nil {
		return receipt, err
	}
	if plan.ID != planID {
		return receipt, retention.ErrPlanChanged
	}
	if len(plan.Blockers) > 0 {
		return receipt, fmt.Errorf("%w: %v", retention.ErrBlocked, plan.Blockers)
	}
	receipt.Plan, receipt.CollectedAt = plan, time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO plugin_revision_tombstones(tool_name,revision,plugin_id,record,record_sha,content_sha,plan_id,collected_at) SELECT tool_name,revision,plugin_id,record,record_sha,content_sha,?,? FROM plugin_revisions WHERE tool_name=? AND revision=?`, planID, receipt.CollectedAt.Format(time.RFC3339Nano), name, revision); err != nil {
		return receipt, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_revisions WHERE tool_name=? AND revision=?`, name, revision); err != nil {
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
	if err := saveMaintenanceReceipt(ctx, tx, planID, "plugin_collection", receipt); err != nil {
		return receipt, err
	}
	if err := tx.Commit(); err != nil {
		return retention.CollectionReceipt{}, err
	}
	return receipt, nil
}

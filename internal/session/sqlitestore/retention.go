package sqlitestore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/retention"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

// RetentionPlan must be called by an offline owner. Preview and apply both use
// the same projection; a preview never changes lifecycle or backfills identity.
func (database *Database) RetentionPlan(ctx context.Context, id session.ID) (retention.Plan, error) {
	tx, err := database.db.BeginTx(ctx, nil)
	if err != nil {
		return retention.Plan{}, err
	}
	defer tx.Rollback()
	return planRetention(ctx, tx, id)
}

// CloseRetention closes executable recovery, not the history projection. The
// caller holds the exclusive runtimeowner lock and must not construct an App.
func (database *Database) CloseRetention(ctx context.Context, id session.ID, planID string, confirmed bool) (retention.Receipt, error) {
	var receipt retention.Receipt
	if !confirmed || planID == "" || id == "" {
		return receipt, errors.New("retention close requires a session, reviewed plan ID and explicit confirmation")
	}
	tx, err := database.db.BeginTx(ctx, nil)
	if err != nil {
		return receipt, err
	}
	defer tx.Rollback()
	found, err := loadMaintenanceReceipt(ctx, tx, planID, "retention", &receipt)
	if err != nil {
		return receipt, err
	}
	if found {
		if receipt.Plan.SessionID != id {
			return retention.Receipt{}, retention.ErrPlanChanged
		}
		return receipt, nil
	}
	plan, err := planRetention(ctx, tx, id)
	if err != nil {
		return receipt, err
	}
	if plan.ID != planID {
		return receipt, retention.ErrPlanChanged
	}
	if len(plan.Blockers) > 0 {
		return receipt, fmt.Errorf("%w: %v", retention.ErrBlocked, plan.Blockers)
	}
	if len(plan.Consumers) == 0 {
		return receipt, errors.New("retention plan has no consumers to close")
	}
	receipt.Plan, receipt.ClosedAt = plan, time.Now().UTC()
	var pending []session.PendingEvent
	for _, consumer := range plan.Consumers {
		ref := consumer.Reference
		if _, err := tx.ExecContext(ctx, `INSERT INTO runtime_consumer_closures(kind,scope,consumer_id,plan_id,closed_at) VALUES(?,?,?,?,?)`, ref.Kind, ref.Scope, ref.ID, planID, receipt.ClosedAt.Format(time.RFC3339Nano)); err != nil {
			return receipt, err
		}
		result, err := tx.ExecContext(ctx, `UPDATE runtime_snapshot_refs SET released_at=?,release_reason=? WHERE kind=? AND scope=? AND consumer_id=? AND released_at IS NULL`, receipt.ClosedAt.Format(time.RFC3339Nano), "archived by retention plan "+planID, ref.Kind, ref.Scope, ref.ID)
		if err != nil {
			return receipt, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return receipt, err
		}
		receipt.ReleasedReferences += int(count)
		if ref.Kind == "turn" {
			pending = append(pending, session.PendingEvent{Type: session.EventTurnArchived, CorrelationID: ref.ID, RuntimeSnapshotID: consumer.SnapshotID, Payload: session.MustJSONPayload(map[string]any{"plan_id": planID, "history_preserved": true, "effects_reverted": false})})
		}
	}
	if len(pending) > 0 {
		if _, err := appendSessionEvents(ctx, tx, id, plan.Sequence, pending...); err != nil {
			return receipt, err
		}
	}
	if err := saveMaintenanceReceipt(ctx, tx, planID, "retention", receipt); err != nil {
		return receipt, err
	}
	if err := tx.Commit(); err != nil {
		return retention.Receipt{}, err
	}
	return receipt, nil
}

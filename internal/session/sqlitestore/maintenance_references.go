package sqlitestore

import (
	"context"
	"database/sql"
	"errors"

	"github.com/vkviyu/go-self-evolving-agent/internal/retention"
)

func closeRetentionRows(rows *sql.Rows) error { return errors.Join(rows.Err(), rows.Close()) }

func loadRetentionReference(ctx context.Context, query rowQuery, consumer *retention.Consumer) (bool, error) {
	ref := consumer.Reference
	if err := ref.Validate(); err != nil {
		return false, err
	}
	var count int
	if err := query.QueryRowContext(ctx, `SELECT COUNT(*) FROM runtime_consumer_closures WHERE kind=? AND scope=? AND consumer_id=?`, ref.Kind, ref.Scope, ref.ID).Scan(&count); err != nil {
		return false, err
	}
	if count != 0 {
		return true, nil
	}
	var snapshot string
	var released sql.NullString
	err := query.QueryRowContext(ctx, `SELECT snapshot_id,released_at FROM runtime_snapshot_refs WHERE kind=? AND scope=? AND consumer_id=?`, ref.Kind, ref.Scope, ref.ID).Scan(&snapshot, &released)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if consumer.SnapshotID != "" && snapshot != consumer.SnapshotID {
		return false, errors.New("consumer and retention reference snapshot identity mismatch")
	}
	consumer.SnapshotID = snapshot
	return released.Valid, nil
}

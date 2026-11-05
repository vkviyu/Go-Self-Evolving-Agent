package sqlitestore

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

type RuntimeSnapshots struct{ db *sql.DB }

var _ tool.SnapshotStore = (*RuntimeSnapshots)(nil)

func (database *Database) RuntimeSnapshots() *RuntimeSnapshots {
	return &RuntimeSnapshots{db: database.db}
}

func (store *RuntimeSnapshots) Save(ctx context.Context, manifest tool.SnapshotManifest, reference tool.SnapshotReference) error {
	if err := reference.Validate(); err != nil {
		return err
	}
	encoded, err := manifest.Encode()
	if err != nil {
		return err
	}
	id, err := manifest.ID()
	if err != nil {
		return err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var closed int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM runtime_consumer_closures WHERE kind = ? AND scope = ? AND consumer_id = ?`, reference.Kind, reference.Scope, reference.ID).Scan(&closed); err != nil {
		return err
	}
	if closed != 0 {
		return tool.ErrSnapshotReferenceReleased
	}
	if reference.Kind == "turn" {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_events WHERE session_id = ? AND correlation_id = ? AND event_type = 'turn.abandoned'`, reference.Scope, reference.ID).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return tool.ErrSnapshotReferenceReleased
		}
	}
	var existing []byte
	err = tx.QueryRowContext(ctx, `SELECT manifest FROM runtime_snapshots WHERE snapshot_id = ?`, id).Scan(&existing)
	if err == nil && !bytes.Equal(existing, encoded) {
		return fmt.Errorf("%w: immutable manifest changed", tool.ErrSnapshotUnavailable)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO runtime_snapshots(snapshot_id, manifest) VALUES(?, ?)`, id, encoded); err != nil {
			return err
		}
	}
	var previous string
	var released sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT snapshot_id, released_at FROM runtime_snapshot_refs WHERE kind = ? AND scope = ? AND consumer_id = ?`, reference.Kind, reference.Scope, reference.ID).Scan(&previous, &released)
	if err == nil && released.Valid {
		return tool.ErrSnapshotReferenceReleased
	}
	if err == nil && previous != id {
		return tool.ErrSnapshotReferenceConflict
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO runtime_snapshot_refs(kind, scope, consumer_id, snapshot_id) VALUES(?, ?, ?, ?)`, reference.Kind, reference.Scope, reference.ID, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (store *RuntimeSnapshots) Load(ctx context.Context, id string) (tool.SnapshotManifest, error) {
	var encoded []byte
	if err := store.db.QueryRowContext(ctx, `SELECT manifest FROM runtime_snapshots WHERE snapshot_id = ?`, id).Scan(&encoded); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return tool.SnapshotManifest{}, fmt.Errorf("%w: %s", tool.ErrSnapshotUnavailable, id)
		}
		return tool.SnapshotManifest{}, err
	}
	manifest, err := tool.DecodeSnapshotManifest(encoded)
	if err != nil {
		return tool.SnapshotManifest{}, fmt.Errorf("%w: decode manifest: %v", tool.ErrSnapshotUnavailable, err)
	}
	actual, err := manifest.ID()
	if err != nil || actual != id {
		return tool.SnapshotManifest{}, fmt.Errorf("%w: manifest digest mismatch", tool.ErrSnapshotUnavailable)
	}
	canonical, err := manifest.Encode()
	if err != nil || !bytes.Equal(canonical, encoded) {
		return tool.SnapshotManifest{}, fmt.Errorf("%w: noncanonical manifest", tool.ErrSnapshotUnavailable)
	}
	return manifest, nil
}

// References reports unreleased consumers, not active tasks or a GC decision.
func (store *RuntimeSnapshots) References(ctx context.Context, id string) ([]tool.SnapshotReference, error) {
	if _, err := store.Load(ctx, id); err != nil {
		return nil, err
	}
	rows, err := store.db.QueryContext(ctx, `SELECT kind, scope, consumer_id FROM runtime_snapshot_refs WHERE snapshot_id = ? AND released_at IS NULL ORDER BY kind, scope, consumer_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var references []tool.SnapshotReference
	for rows.Next() {
		var reference tool.SnapshotReference
		if err := rows.Scan(&reference.Kind, &reference.Scope, &reference.ID); err != nil {
			return nil, err
		}
		if err := reference.Validate(); err != nil {
			return nil, err
		}
		references = append(references, reference)
	}
	return references, rows.Err()
}

// ReferenceHistory preserves release evidence; released rows are not deleted.
func (store *RuntimeSnapshots) ReferenceHistory(ctx context.Context, id string) ([]tool.SnapshotReferenceRecord, error) {
	if _, err := store.Load(ctx, id); err != nil {
		return nil, err
	}
	rows, err := store.db.QueryContext(ctx, `SELECT kind, scope, consumer_id, released_at, release_reason FROM runtime_snapshot_refs WHERE snapshot_id = ? ORDER BY kind, scope, consumer_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []tool.SnapshotReferenceRecord
	for rows.Next() {
		var record tool.SnapshotReferenceRecord
		var released sql.NullString
		if err := rows.Scan(&record.Reference.Kind, &record.Reference.Scope, &record.Reference.ID, &released, &record.ReleaseReason); err != nil {
			return nil, err
		}
		if err := record.Reference.Validate(); err != nil {
			return nil, err
		}
		if released.Valid {
			at, err := time.Parse(time.RFC3339Nano, released.String)
			if err != nil {
				return nil, err
			}
			record.ReleasedAt = &at
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

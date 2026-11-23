package runtimestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/retention"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

type RuntimeSnapshots struct {
	db    *sql.DB
	guard func(context.Context, tool.SnapshotReference) error
}

var ErrSnapshotAuthorityUnavailable = errors.New("runtime snapshot save requires an authoritative reference guard")

// WithReferenceGuard binds a fresh handle to authoritative lifecycle proof.
// The host must hold exclusive runtime ownership across guard and local commit;
// this is not a transaction spanning ATE and SQLite.
func (store *RuntimeSnapshots) WithReferenceGuard(guard func(context.Context, tool.SnapshotReference) error) *RuntimeSnapshots {
	return &RuntimeSnapshots{db: store.db, guard: guard}
}

var _ tool.SnapshotStore = (*RuntimeSnapshots)(nil)

func (database *Database) RuntimeSnapshots() *RuntimeSnapshots {
	return &RuntimeSnapshots{db: database.db}
}

func (store *RuntimeSnapshots) Save(ctx context.Context, manifest tool.SnapshotManifest, reference tool.SnapshotReference) error {
	if err := reference.Validate(); err != nil {
		return err
	}
	if store.guard == nil {
		return ErrSnapshotAuthorityUnavailable
	}
	if err := store.guard(ctx, reference); err != nil {
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

// CloseAbandonedReferences performs local cleanup only after the trusted host
// has obtained a completed authoritative abandonment receipt. It preserves
// manifests and reference evidence, and closes even consumers with no reference
// yet. Repeating cleanup returns the same released-reference count.
func (store *RuntimeSnapshots) CloseAbandonedReferences(ctx context.Context, receipt session.Abandonment) (int, error) {
	if receipt.SessionID == "" || receipt.Reason == "" || receipt.AbandonedAt.IsZero() || receipt.EffectsReverted {
		return 0, errors.New("invalid authoritative abandonment receipt")
	}
	receipt.ReleasedReferences = 0
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return 0, err
	}
	identity := fmt.Sprintf("abandon:%x", sha256.Sum256(encoded))
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	count := 0
	seen := make(map[string]bool)
	for _, id := range receipt.TurnIDs {
		if id == "" || seen[id] {
			return 0, errors.New("invalid abandonment turn identities")
		}
		seen[id] = true
		var previous string
		err := tx.QueryRowContext(ctx, `SELECT plan_id FROM runtime_consumer_closures WHERE kind='turn' AND scope=? AND consumer_id=?`, string(receipt.SessionID), id).Scan(&previous)
		if err == nil && previous != identity {
			return 0, errors.New("runtime consumer was closed by a different operation")
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		if errors.Is(err, sql.ErrNoRows) {
			var released sql.NullString
			err := tx.QueryRowContext(ctx, `SELECT released_at FROM runtime_snapshot_refs WHERE kind='turn' AND scope=? AND consumer_id=?`, string(receipt.SessionID), id).Scan(&released)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return 0, err
			}
			if released.Valid {
				return 0, errors.New("runtime reference released without matching cleanup identity")
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO runtime_consumer_closures(kind,scope,consumer_id,plan_id,closed_at) VALUES('turn',?,?,?,?)`, string(receipt.SessionID), id, identity, receipt.AbandonedAt.Format(time.RFC3339Nano)); err != nil {
				return 0, err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE runtime_snapshot_refs SET released_at=?, release_reason=? WHERE kind='turn' AND scope=? AND consumer_id=? AND released_at IS NULL`, receipt.AbandonedAt.Format(time.RFC3339Nano), receipt.Reason, string(receipt.SessionID), id); err != nil {
			return 0, err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM runtime_snapshot_refs WHERE kind='turn' AND scope=? AND consumer_id=?`, string(receipt.SessionID), id).Scan(&n); err != nil {
			return 0, err
		}
		count += n
	}
	return count, tx.Commit()
}

// SessionTurnReferences enumerates runtime identities, never SQL business history.
func (store *RuntimeSnapshots) SessionTurnReferences(ctx context.Context, id session.ID) ([]retention.Consumer, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT consumer_id, snapshot_id FROM runtime_snapshot_refs WHERE kind='turn' AND scope=? AND released_at IS NULL ORDER BY consumer_id LIMIT 1001`, string(id))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var consumers []retention.Consumer
	for rows.Next() {
		consumer := retention.Consumer{Reference: tool.SnapshotReference{Kind: "turn", Scope: string(id)}}
		if err := rows.Scan(&consumer.Reference.ID, &consumer.SnapshotID); err != nil {
			return nil, err
		}
		if err := consumer.Reference.Validate(); err != nil {
			return nil, err
		}
		consumers = append(consumers, consumer)
		if len(consumers) > 1000 {
			return nil, errors.New("runtime retention reference limit exceeded")
		}
	}
	return consumers, rows.Err()
}

// CloseArchivedReferences accepts a completed authority receipt from the host.
// All local releases share one transaction; manifests and reference rows remain.
func (store *RuntimeSnapshots) CloseArchivedReferences(ctx context.Context, receipt retention.Receipt) (int, error) {
	plan := receipt.Plan
	if plan.SessionID == "" || plan.ID == "" || plan.ID != plan.Digest() ||
		len(plan.Blockers) != 0 || len(plan.Consumers) == 0 || receipt.ClosedAt.IsZero() || receipt.EvidenceDeleted {
		return 0, errors.New("invalid authoritative archive receipt")
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	count := 0
	seen := make(map[string]bool)
	identity := "archive:" + plan.ID
	for _, consumer := range plan.Consumers {
		ref := consumer.Reference
		if err := ref.Validate(); err != nil {
			return 0, err
		}
		if ref.Kind != "turn" || ref.Scope != string(plan.SessionID) || seen[ref.ID] {
			return 0, errors.New("invalid archive consumer")
		}
		seen[ref.ID] = true
		var previous string
		err := tx.QueryRowContext(ctx, `SELECT plan_id FROM runtime_consumer_closures WHERE kind='turn' AND scope=? AND consumer_id=?`, ref.Scope, ref.ID).Scan(&previous)
		existing := err == nil
		if existing && previous != identity {
			return 0, errors.New("runtime consumer was closed by a different operation")
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		var snapshot string
		var released sql.NullString
		err = tx.QueryRowContext(ctx, `SELECT snapshot_id, released_at FROM runtime_snapshot_refs WHERE kind='turn' AND scope=? AND consumer_id=?`, ref.Scope, ref.ID).Scan(&snapshot, &released)
		found := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		if snapshot != consumer.SnapshotID || (released.Valid && !existing) || (found && snapshot == "") {
			return 0, errors.New("runtime reference differs from authoritative archive proof")
		}
		if !existing {
			if _, err := tx.ExecContext(ctx, `INSERT INTO runtime_consumer_closures(kind,scope,consumer_id,plan_id,closed_at) VALUES('turn',?,?,?,?)`, ref.Scope, ref.ID, identity, receipt.ClosedAt.Format(time.RFC3339Nano)); err != nil {
				return 0, err
			}
		}
		if found {
			if _, err := tx.ExecContext(ctx, `UPDATE runtime_snapshot_refs SET released_at=?,release_reason=? WHERE kind='turn' AND scope=? AND consumer_id=? AND released_at IS NULL`, receipt.ClosedAt.Format(time.RFC3339Nano), identity, ref.Scope, ref.ID); err != nil {
				return 0, err
			}
			count++
		}
	}
	return count, tx.Commit()
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

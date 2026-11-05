package workspacedomain

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const (
	databaseName = "coord.db"
	// timeLayout is fixed-width so stored timestamps sort chronologically.
	timeLayout = "2006-01-02T15:04:05.000000000Z"
	// historyRetention bounds how long finished leases and instances remain.
	historyRetention = 24 * time.Hour
)

// schemaMigrations are applied in order; the database user_version records
// how many have been applied. Published entries must never change.
var schemaMigrations = []string{`
CREATE TABLE meta (
    key TEXT PRIMARY KEY,
    value NOT NULL
);
INSERT INTO meta(key, value) VALUES('lease_epoch', 0);
CREATE TABLE instances (
    instance_id TEXT PRIMARY KEY,
    pid INTEGER NOT NULL,
    hostname TEXT NOT NULL,
    data_directory TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('live', 'closed', 'dead')),
    started_at TEXT NOT NULL,
    ended_at TEXT
);
CREATE INDEX idx_instances_state ON instances(state);
CREATE TABLE leases (
    lease_id TEXT PRIMARY KEY,
    instance_id TEXT NOT NULL REFERENCES instances(instance_id),
    team_id TEXT NOT NULL,
    agent_id TEXT NOT NULL,
    run_id TEXT NOT NULL DEFAULT '',
    path TEXT NOT NULL,
    path_key TEXT NOT NULL,
    mode TEXT NOT NULL CHECK (mode IN ('read', 'write')),
    state TEXT NOT NULL CHECK (state IN ('active', 'released', 'abandoned')),
    epoch INTEGER NOT NULL,
    correlation_id TEXT NOT NULL DEFAULT '',
    idempotency_key TEXT NOT NULL DEFAULT '',
    request_hash TEXT NOT NULL,
    release_reason TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    released_at TEXT
);
CREATE INDEX idx_leases_active_path ON leases(path_key) WHERE state = 'active';
CREATE INDEX idx_leases_instance_state ON leases(instance_id, state);
CREATE INDEX idx_leases_team_agent ON leases(team_id, agent_id);
CREATE INDEX idx_leases_idempotency ON leases(team_id, idempotency_key, created_at) WHERE idempotency_key <> '';
`}

// openDatabase opens the coordination database shared by every instance of
// one GSEA_HOME on one workspace. Concurrent first opens cannot all convert a
// new database to WAL, so callers hold the home-wide domains mutex while the
// connection is established and migrated. Write transactions begin IMMEDIATE
// so instances queue on the write lock instead of failing a lock upgrade.
func openDatabase(ctx context.Context, path string) (*sql.DB, error) {
	location := &url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	query := location.Query()
	query.Add("_pragma", "foreign_keys(ON)")
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "journal_mode(WAL)")
	query.Set("_txlock", "immediate")
	query.Set("_defensive", "1")
	location.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", location.String())
	if err != nil {
		return nil, fmt.Errorf("open workspace coordination database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	// The migration transaction is the first use, so the connection and its
	// pragmas are established while the caller holds the mutex.
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// migrate applies pending schema versions in one write transaction. Reading
// user_version inside that transaction lets instances opening a new database
// at the same time apply each version exactly once.
func migrate(ctx context.Context, db *sql.DB) error {
	return withTx(ctx, db, "coordination schema migration", func(tx *sql.Tx) error {
		var version int
		if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
			return fmt.Errorf("read coordination schema version: %w", err)
		}
		if version > len(schemaMigrations) {
			return fmt.Errorf("coordination schema %d is newer than supported schema %d", version, len(schemaMigrations))
		}
		if version == len(schemaMigrations) {
			return nil
		}
		for index := version; index < len(schemaMigrations); index++ {
			if _, err := tx.ExecContext(ctx, schemaMigrations[index]); err != nil {
				return fmt.Errorf("apply coordination schema %d: %w", index+1, err)
			}
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, len(schemaMigrations))); err != nil {
			return fmt.Errorf("record coordination schema version: %w", err)
		}
		return nil
	})
}

// registerInstance records this process as a live holder and remembers the
// workspace path for people inspecting the coordination directory.
func registerInstance(ctx context.Context, db *sql.DB, instanceID, hostname, dataDirectory, workspaceRoot string, pid int) error {
	return withTx(ctx, db, "instance registration", func(tx *sql.Tx) error {
		now := formatTime(time.Now())
		if _, err := tx.ExecContext(ctx, `INSERT INTO instances(instance_id, pid, hostname, data_directory, state, started_at) VALUES(?, ?, ?, ?, 'live', ?)`,
			instanceID, pid, hostname, dataDirectory, now); err != nil {
			return fmt.Errorf("register coordination instance: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES('workspace_root', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, workspaceRoot); err != nil {
			return fmt.Errorf("record coordination workspace: %w", err)
		}
		return nil
	})
}

// closeInstance releases every lease this instance still holds and marks it
// closed. Only a clean shutdown calls it; crashed instances are reclaimed.
func closeInstance(ctx context.Context, db *sql.DB, instanceID string) error {
	return withTx(ctx, db, "instance shutdown", func(tx *sql.Tx) error {
		now := formatTime(time.Now())
		if _, err := tx.ExecContext(ctx, `UPDATE leases SET state = 'released', release_reason = 'instance closed', updated_at = ?, released_at = ? WHERE instance_id = ? AND state = 'active'`,
			now, now, instanceID); err != nil {
			return fmt.Errorf("release instance leases: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE instances SET state = 'closed', ended_at = ? WHERE instance_id = ? AND state = 'live'`, now, instanceID); err != nil {
			return fmt.Errorf("close coordination instance: %w", err)
		}
		return nil
	})
}

// pruneHistory deletes finished leases and instances older than the
// retention window. Active leases and live instances are never pruned.
func pruneHistory(ctx context.Context, db *sql.DB, now time.Time) error {
	return withTx(ctx, db, "coordination history pruning", func(tx *sql.Tx) error {
		cutoff := formatTime(now.Add(-historyRetention))
		if _, err := tx.ExecContext(ctx, `DELETE FROM leases WHERE state <> 'active' AND updated_at < ?`, cutoff); err != nil {
			return fmt.Errorf("prune finished leases: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM instances WHERE state <> 'live' AND ended_at < ? AND NOT EXISTS (SELECT 1 FROM leases WHERE leases.instance_id = instances.instance_id)`, cutoff); err != nil {
			return fmt.Errorf("prune finished instances: %w", err)
		}
		return nil
	})
}

// withTx runs fn in one write transaction and commits it when fn succeeds.
// purpose names the transaction in begin and commit errors.
func withTx(ctx context.Context, db *sql.DB, purpose string, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin %s: %w", purpose, err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit %s: %w", purpose, err)
	}
	return nil
}

// formatTime encodes UTC timestamps with the fixed-width layout.
func formatTime(value time.Time) string {
	return value.UTC().Format(timeLayout)
}

// parseTime decodes a timestamp written by formatTime.
func parseTime(value string) (time.Time, error) {
	return time.Parse(timeLayout, value)
}

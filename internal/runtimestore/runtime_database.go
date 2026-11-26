package runtimestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// runtimeDDL contains host execution resources only. Session history, tasks,
// messages, usage facts and delivery outboxes are deliberately absent.
const runtimeDDL = `
CREATE TABLE runtime_schema (
    version INTEGER PRIMARY KEY,
    checksum TEXT NOT NULL
);
CREATE TABLE runtime_authority (
    singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
    store_id TEXT NOT NULL
);
CREATE TABLE agent_checkpoints (
    checkpoint_id TEXT PRIMARY KEY,
    checkpoint BLOB NOT NULL,
    checksum TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE runtime_snapshots (
    snapshot_id TEXT PRIMARY KEY,
    manifest BLOB NOT NULL CHECK(length(manifest) > 0 AND length(manifest) <= 1048576)
);
CREATE TABLE runtime_snapshot_refs (
    kind TEXT NOT NULL CHECK(kind IN ('turn', 'flow')),
    scope TEXT NOT NULL, consumer_id TEXT NOT NULL,
    snapshot_id TEXT NOT NULL REFERENCES runtime_snapshots(snapshot_id),
    released_at TEXT, release_reason TEXT NOT NULL DEFAULT '',
    PRIMARY KEY(kind, scope, consumer_id)
);
CREATE INDEX runtime_snapshot_ref_lookup ON runtime_snapshot_refs(snapshot_id);
CREATE TABLE runtime_consumer_closures (
    kind TEXT NOT NULL CHECK(kind IN ('turn', 'flow')),
    scope TEXT NOT NULL, consumer_id TEXT NOT NULL, plan_id TEXT NOT NULL,
    closed_at TEXT NOT NULL, PRIMARY KEY(kind, scope, consumer_id)
);
`

// The v1 and v2 checksums cover the exact DDL from d0ff9dd and 9f1cc82.
// Layout fingerprints verify the actual schema without reading retired data.
const (
	runtimeSchemaVersion = 3
	runtimeV1Checksum    = "2d34361501dd90b7861cd49c89e1ea0248c59e5381388178210acf7130833f20"
	runtimeV1Layout      = "254e3f85905185f814b484e6630729efbcbc412410d9ccba73705af779cd5063"
	runtimeV2Checksum    = "f43ca290c3c2b33066f94215c2822f469b7a8fd6a7e3106efd7a9424636333b0"
	runtimeV2Layout      = "6e776ff7ae91b30a3855dc4ad9ff3cf78a7561dae43507fc5e61ebff0305450c"
)

// OpenRuntime opens ATE-linked host runtime databases. Known older formats keep
// their schema marker and all historical evidence in place.
func OpenRuntime(ctx context.Context, path string) (*Database, error) {
	if path == "" {
		return nil, errors.New("runtime database path is required")
	}
	dsn, err := dataSourceName(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open runtime database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	database := &Database{db: db}
	if err := database.initializeRuntime(ctx); err != nil {
		_ = database.Close()
		return nil, err
	}
	return database, nil
}

// initializeRuntime publishes schema and checksum together and checks existing
// files before any DDL. Unknown or mixed formats are never repaired implicitly.
func (database *Database) initializeRuntime(ctx context.Context) error {
	tx, err := database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var initialized, objects int
	if err := tx.QueryRowContext(ctx, `SELECT
        COALESCE(SUM(type='table' AND name='runtime_schema'), 0), COUNT(*)
        FROM sqlite_master WHERE name NOT GLOB 'sqlite_*'`).Scan(&initialized, &objects); err != nil {
		return err
	}
	if initialized == 0 && objects != 0 {
		return errors.New("existing database is not an ATE-linked runtime database; retain it separately and use a new runtime-v3.db")
	}
	ddl := runtimeDDL + ateRequestsDDL + ateRequestRejectionsDDL
	checksum := sha256Hex([]byte(ddl))
	if initialized != 0 {
		var version int
		var stored string
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MIN(version),0),COALESCE(MIN(checksum),'') FROM runtime_schema`).Scan(&count, &version, &stored); err != nil {
			return err
		}
		var wantChecksum, wantLayout string
		switch version {
		case 1:
			wantChecksum, wantLayout = runtimeV1Checksum, runtimeV1Layout
		case 2:
			wantChecksum, wantLayout = runtimeV2Checksum, runtimeV2Layout
		case runtimeSchemaVersion:
			wantChecksum, wantLayout = checksum, runtimeLayoutFromDDL(ddl)
		}
		if count != 1 || wantChecksum == "" || stored != wantChecksum {
			return errors.New("runtime database schema version or checksum mismatch")
		}
		if err := validateRuntimeSchema(ctx, tx, wantLayout); err != nil {
			return err
		}
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("initialize runtime schema: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO runtime_schema(version, checksum) VALUES(?, ?)`, runtimeSchemaVersion, checksum); err != nil {
		return err
	}
	return tx.Commit()
}

// runtimeLayoutFromDDL fingerprints the named objects of the current DDL.
func runtimeLayoutFromDDL(ddl string) string {
	var entries []string
	for _, statement := range strings.Split(ddl, ";") {
		fields := strings.Fields(statement)
		for i, field := range fields {
			if (field == "TABLE" || field == "INDEX") && i+1 < len(fields) {
				entries = append(entries, fields[i+1]+"\x00"+strings.Join(fields, " "))
				break
			}
		}
	}
	sort.Strings(entries)
	return sha256Hex([]byte(strings.Join(entries, "\n")))
}

// validateRuntimeSchema checks actual objects, not just the stored marker.
// Triggers may exist for operational fault tests.
func validateRuntimeSchema(ctx context.Context, tx *sql.Tx, wantLayout string) error {
	rows, err := tx.QueryContext(ctx, `SELECT name,sql FROM sqlite_master
        WHERE type<>'trigger' AND name NOT GLOB 'sqlite_*' ORDER BY name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var entries []string
	for rows.Next() {
		var name, statement string
		if err := rows.Scan(&name, &statement); err != nil {
			return err
		}
		entries = append(entries, name+"\x00"+strings.Join(strings.Fields(statement), " "))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if sha256Hex([]byte(strings.Join(entries, "\n"))) != wantLayout {
		return errors.New("runtime database has incomplete, unexpected or altered schema objects; retain it separately")
	}
	return nil
}

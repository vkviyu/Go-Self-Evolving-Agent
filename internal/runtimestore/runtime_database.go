package runtimestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
CREATE TABLE lsp_diagnostic_projections (
    workspace_root TEXT NOT NULL, path TEXT NOT NULL, revision TEXT NOT NULL,
    report BLOB NOT NULL, updated_at TEXT NOT NULL,
    PRIMARY KEY(workspace_root, path, revision)
);
CREATE INDEX idx_lsp_diagnostics_latest
    ON lsp_diagnostic_projections(workspace_root, path, updated_at DESC);
CREATE TABLE plugin_blobs (
    content_sha TEXT PRIMARY KEY,
    source BLOB NOT NULL CHECK(length(source) > 0 AND length(source) <= 33554432)
);
CREATE TABLE plugin_revisions (
    tool_name TEXT NOT NULL, revision TEXT NOT NULL, plugin_id TEXT NOT NULL,
    record BLOB NOT NULL CHECK(length(record) <= 65536),
    record_sha TEXT NOT NULL,
    content_sha TEXT NOT NULL REFERENCES plugin_blobs(content_sha),
    state TEXT NOT NULL CHECK(state IN ('staged', 'active', 'retired')),
    PRIMARY KEY(tool_name, revision)
);
CREATE UNIQUE INDEX plugin_one_active_revision ON plugin_revisions(tool_name) WHERE state = 'active';
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
CREATE TABLE plugin_revision_tombstones (
    tool_name TEXT NOT NULL, revision TEXT NOT NULL, plugin_id TEXT NOT NULL,
    record BLOB NOT NULL, record_sha TEXT NOT NULL, content_sha TEXT NOT NULL,
    plan_id TEXT NOT NULL, collected_at TEXT NOT NULL,
    PRIMARY KEY(tool_name, revision)
);
`

// OpenRuntime opens the clean host runtime format. It refuses legacy databases
// instead of applying historical business migrations or backfilling an outbox.
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
		if err := validateRuntimeSchema(ctx, tx, ddl); err != nil {
			return err
		}
		var version int
		var stored string
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MIN(version),0),COALESCE(MIN(checksum),'') FROM runtime_schema`).Scan(&count, &version, &stored); err != nil {
			return err
		}
		if count != 1 || version != 1 || stored != checksum {
			return errors.New("runtime database schema version or checksum mismatch")
		}
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("initialize runtime schema: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO runtime_schema(version, checksum) VALUES(1, ?)`, checksum); err != nil {
		return err
	}
	return tx.Commit()
}

// Compare the actual tables/indexes with the clean format, not just a self-
// reported checksum. Extra tables/views and incomplete or altered schemas are
// rejected without migrations. Triggers may exist for operational fault tests.
func validateRuntimeSchema(ctx context.Context, tx *sql.Tx, ddl string) error {
	expected := make(map[string]string)
	for _, statement := range strings.Split(ddl, ";") {
		fields := strings.Fields(statement)
		for i, field := range fields {
			if (field == "TABLE" || field == "INDEX") && i+1 < len(fields) {
				expected[fields[i+1]] = strings.Join(fields, " ")
				break
			}
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT name,sql FROM sqlite_master WHERE type<>'trigger' AND name NOT GLOB 'sqlite_*'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name, statement string
		if err := rows.Scan(&name, &statement); err != nil {
			return err
		}
		want, exists := expected[name]
		if !exists || strings.Join(strings.Fields(statement), " ") != want {
			return fmt.Errorf("runtime database contains unexpected or altered schema object %q; retain it separately", name)
		}
		delete(expected, name)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(expected) != 0 {
		return errors.New("runtime database schema is incomplete; retain it separately")
	}
	return nil
}

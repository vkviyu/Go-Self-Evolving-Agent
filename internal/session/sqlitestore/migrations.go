package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"
)

type migration struct {
	version int
	ddl     string
}

func (database *Database) migrate(ctx context.Context) error {
	if _, err := database.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    checksum TEXT NOT NULL,
    applied_at TEXT NOT NULL
)`); err != nil {
		return fmt.Errorf("bootstrap schema migrations: %w", err)
	}
	var current int
	if err := database.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("read database schema version: %w", err)
	}
	if current > len(migrations) {
		return fmt.Errorf("database schema %d is newer than supported schema %d", current, len(migrations))
	}
	for _, item := range migrations {
		checksum := migrationChecksum(item.ddl)
		if item.version <= current {
			var stored string
			if err := database.db.QueryRowContext(ctx, `SELECT checksum FROM schema_migrations WHERE version = ?`, item.version).Scan(&stored); err != nil {
				return fmt.Errorf("read migration %d checksum: %w", item.version, err)
			}
			if stored != checksum {
				return fmt.Errorf("database migration %d checksum mismatch", item.version)
			}
			continue
		}
		if err := database.applyMigration(ctx, item); err != nil {
			return err
		}
	}
	return validateLegacySessionSchema(ctx, database.db)
}

// Each version owns its transaction: DDL and its ledger entry commit together.
// Do not batch versions or let domain stores initiate migrations independently.
func (database *Database) applyMigration(ctx context.Context, item migration) error {
	tx, err := database.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin database migration %d: %w", item.version, err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, item.ddl); err != nil {
		return fmt.Errorf("apply database migration %d: %w", item.version, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, checksum, applied_at) VALUES(?, ?, ?)`,
		item.version, migrationChecksum(item.ddl), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("record database migration %d: %w", item.version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit database migration %d: %w", item.version, err)
	}
	return nil
}

func validateLegacySessionSchema(ctx context.Context, db rowQuery) error {
	var stored string
	err := db.QueryRowContext(ctx, `SELECT value FROM schema_meta WHERE key = 'session_events'`).Scan(&stored)
	if err != nil {
		return fmt.Errorf("read session event schema version: %w", err)
	}
	version, err := strconv.ParseUint(stored, 10, 32)
	if err != nil {
		return fmt.Errorf("parse session event schema version %q: %w", stored, err)
	}
	if version > uint64(sessionEventSchemaVersion) {
		return fmt.Errorf("session event schema %d is newer than supported schema %d", version, sessionEventSchemaVersion)
	}
	if version < uint64(sessionEventSchemaVersion) {
		return fmt.Errorf("session event schema %d requires a migration to schema %d", version, sessionEventSchemaVersion)
	}
	return nil
}

func migrationChecksum(ddl string) string {
	return sha256Hex([]byte(ddl))
}

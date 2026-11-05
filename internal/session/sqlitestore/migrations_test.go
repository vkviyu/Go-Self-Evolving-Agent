package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

// The initial versions were captured before F4; every later migration appends
// its checksum when published. Existing entries are a durable contract, not a
// golden file that may be regenerated whenever the implementation changes.
func publishedMigrationChecksums(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("testdata/migrations.sha256")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	checksums := make([]string, len(lines))
	for index, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != strconv.Itoa(index+1) || len(fields[1]) != 64 {
			t.Fatalf("invalid published migration fixture line %d: %q", index+1, line)
		}
		checksums[index] = fields[1]
	}
	return checksums
}

func TestPublishedMigrationContract(t *testing.T) {
	checksums := publishedMigrationChecksums(t)
	if len(migrations) != len(checksums) {
		t.Fatalf("every migration must have a pinned published checksum: got %d migrations, %d checksums", len(migrations), len(checksums))
	}
	for index, item := range migrations {
		if item.version != index+1 {
			t.Fatalf("migration at index %d has version %d", index, item.version)
		}
		if index < len(checksums) && migrationChecksum(item.ddl) != checksums[index] {
			t.Errorf("published migration %d changed", item.version)
		}
	}
}

func legacyMigrationDatabase(t *testing.T, version int) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.db")
	dsn, err := dataSourceName(path)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE schema_migrations (
    version INTEGER PRIMARY KEY,
    checksum TEXT NOT NULL,
    applied_at TEXT NOT NULL
)`); err != nil {
		t.Fatal(err)
	}
	checksums := publishedMigrationChecksums(t)
	for _, item := range migrations[:version] {
		if _, err := db.Exec(item.ddl); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations VALUES(?,?,?)`, item.version, checksums[item.version-1], "2026-01-01T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	return path, db
}

func databaseSchema(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT type,name,tbl_name,COALESCE(sql,'') FROM sqlite_master ORDER BY type,name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var schema []string
	for rows.Next() {
		var kind, name, table, ddl string
		if err := rows.Scan(&kind, &name, &table, &ddl); err != nil {
			t.Fatal(err)
		}
		schema = append(schema, strings.Join([]string{kind, name, table, ddl}, "\n"))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return schema
}

func TestMigrationUpgradeFromEveryPublishedVersion(t *testing.T) {
	ctx := context.Background()
	fresh, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fresh.Close() })
	wantSchema := databaseSchema(t, fresh.db)
	for version := 0; version <= len(publishedMigrationChecksums(t)); version++ {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			path, legacy := legacyMigrationDatabase(t, version)
			var original []session.Event
			if version > 0 {
				original, err = (&SessionEventStore{db: legacy}).Append(ctx, "legacy", 0, session.PendingEvent{Type: session.EventUserMessage, Payload: []byte(`{"content":"preserve history"}`)})
				if err != nil {
					t.Fatal(err)
				}
				if err := (&CheckpointStore{db: legacy}).Set(ctx, "legacy-checkpoint", []byte("opaque framework bytes")); err != nil {
					t.Fatal(err)
				}
			}
			if err := legacy.Close(); err != nil {
				t.Fatal(err)
			}
			upgraded, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = upgraded.Close() })
			if got := databaseSchema(t, upgraded.db); !reflect.DeepEqual(got, wantSchema) {
				t.Fatal("upgraded schema differs from fresh database")
			}
			var preserved int
			if err := upgraded.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE applied_at='2026-01-01T00:00:00Z'`).Scan(&preserved); err != nil || preserved != version {
				t.Fatalf("published ledger rewritten: preserved=%d err=%v", preserved, err)
			}
			if version > 0 {
				got, err := upgraded.SessionEvents().Load(ctx, "legacy", 0, 10)
				if err != nil || !reflect.DeepEqual(got, original) {
					t.Fatalf("history changed: got=%+v want=%+v err=%v", got, original, err)
				}
				checkpoint, found, err := upgraded.Checkpoints().Get(ctx, "legacy-checkpoint")
				if err != nil || !found || string(checkpoint) != "opaque framework bytes" {
					t.Fatalf("checkpoint changed: %q found=%v err=%v", checkpoint, found, err)
				}
			}
		})
	}
}

func TestMigrationFailureRollsBackDDLAndLedger(t *testing.T) {
	path, legacy := legacyMigrationDatabase(t, 1)
	if _, err := legacy.Exec(`CREATE TRIGGER fail_migration BEFORE INSERT ON schema_migrations WHEN NEW.version=2 BEGIN SELECT RAISE(ABORT,'injected ledger failure'); END`); err != nil {
		t.Fatal(err)
	}
	before := databaseSchema(t, legacy)
	if database, err := Open(context.Background(), path); err == nil {
		_ = database.Close()
		t.Fatal("migration succeeded without recording its checksum")
	} else if !strings.Contains(err.Error(), "record database migration 2") {
		t.Fatal(err)
	}
	if got := databaseSchema(t, legacy); !reflect.DeepEqual(got, before) {
		t.Fatal("failed migration left partial DDL")
	}
	var count int
	if err := legacy.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("ledger after rollback: count=%d err=%v", count, err)
	}
	if _, err := legacy.Exec(`DROP TRIGGER fail_migration`); err != nil {
		t.Fatal(err)
	}
	database, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
	_ = database.Close()
}

func TestMigrationRejectsIncompatibleLedger(t *testing.T) {
	for _, test := range []struct{ name, statement, want string }{
		{"checksum", `UPDATE schema_migrations SET checksum='altered' WHERE version=1`, "migration 1 checksum mismatch"},
		{"gap", `DELETE FROM schema_migrations WHERE version=1`, "read migration 1 checksum"},
		{"future", `INSERT INTO schema_migrations VALUES(999,'future','timestamp')`, "newer than supported schema"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path, legacy := legacyMigrationDatabase(t, 2)
			if _, err := legacy.Exec(test.statement); err != nil {
				t.Fatal(err)
			}
			before := databaseSchema(t, legacy)
			if database, err := Open(context.Background(), path); err == nil {
				_ = database.Close()
				t.Fatal("invalid ledger accepted")
			} else if !strings.Contains(err.Error(), test.want) {
				t.Fatal(err)
			}
			if got := databaseSchema(t, legacy); !reflect.DeepEqual(got, before) {
				t.Fatal("invalid ledger caused schema changes")
			}
		})
	}
}

func TestMigrationDDLFailureRollsBackEarlierStatements(t *testing.T) {
	path, legacy := legacyMigrationDatabase(t, 1)
	// Version 2 creates its event and team tables before creating agents. This
	// collision must roll back those earlier statements, not only the ledger.
	if _, err := legacy.Exec(`CREATE TABLE agents (test_collision TEXT)`); err != nil {
		t.Fatal(err)
	}
	before := databaseSchema(t, legacy)
	rowsBefore := snapshotSQLiteRows(t, context.Background(), legacy)
	if database, err := Open(context.Background(), path); err == nil {
		_ = database.Close()
		t.Fatal("accepted conflicting DDL")
	} else if !strings.Contains(err.Error(), "apply database migration 2") {
		t.Fatal(err)
	}
	if got := databaseSchema(t, legacy); !reflect.DeepEqual(got, before) {
		t.Fatal("failed DDL left tables or indexes from its earlier statements")
	}
	if got := snapshotSQLiteRows(t, context.Background(), legacy); got != rowsBefore {
		t.Fatal("failed DDL changed existing rows")
	}
	if _, err := legacy.Exec(`DROP TABLE agents`); err != nil {
		t.Fatal(err)
	}
	database, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("retry after DDL rollback: %v", err)
	}
	_ = database.Close()
}

package runtimestore

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
)

// TestRuntimeDatabaseHasNoBusinessHistory makes accidental SQL fallback fail
// at the storage boundary rather than silently creating a second authority.
func TestRuntimeDatabaseHasNoBusinessHistory(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "runtime.db")
	db, err := OpenRuntime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{
		"session_events", "orchestration_events", "teams", "agents", "tasks",
		"agent_threads", "mailbox_messages", "goal_runs", "flow_runs",
		"usage_records", "temporal_outbox", "temporal_journal", "temporal_state",
		"maintenance_receipts",
		"plugin_builds", "plugin_test_reports", "strategy_specs", "strategy_suites",
		"strategy_reports", "strategy_active", "strategy_events", "strategy_quarantine",
	} {
		var count int
		if err := db.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("runtime database contains business table %s", table)
		}
	}
	payload := []byte("opaque runtime checkpoint")
	if err := db.Checkpoints().Set(ctx, "ate-turn-reference", payload); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenRuntime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, found, err := reopened.Checkpoints().Get(ctx, "ate-turn-reference")
	if err != nil || !found || !bytes.Equal(got, payload) {
		t.Fatalf("checkpoint after reopen: found=%v err=%v", found, err)
	}
}

// TestRuntimeAuthorityBindingSurvivesReopen prevents reusing checkpoints when
// an ATE directory is replaced with a different initialized store.
func TestRuntimeAuthorityBindingSurvivesReopen(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "runtime.db")
	db, err := OpenRuntime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ATERequests().BindAuthority(ctx, "original-store"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = OpenRuntime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.ATERequests().BindAuthority(ctx, "replacement-store"); !errors.Is(err, aterequest.ErrConflict) {
		t.Fatalf("replacement authority accepted: %v", err)
	}
	if err := db.ATERequests().BindAuthority(ctx, "original-store"); err != nil {
		t.Fatalf("original binding changed: %v", err)
	}
}

// TestRuntimeDatabaseRefusesLegacy verifies that startup cannot backfill,
// relabel, or erase a preexisting business history database.
func TestRuntimeDatabaseRefusesLegacy(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		for _, table := range []string{"schema_migrations", "session_events", "tasks", "temporal_journal", "unknown_business"} {
			t.Run(fmt.Sprintf("%s/mixed=%v", table, mixed), func(t *testing.T) {
				ctx := t.Context()
				path := filepath.Join(t.TempDir(), "existing.db")
				if mixed {
					db, err := OpenRuntime(ctx, path)
					if err != nil {
						t.Fatal(err)
					}
					if err := db.Close(); err != nil {
						t.Fatal(err)
					}
				}
				// Create only a foreign format fixture, never invoke a business adapter.
				raw, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				defer raw.Close()
				if _, err := raw.Exec(`CREATE TABLE ` + table + `(payload BLOB); INSERT INTO ` + table + ` VALUES(x'00ff')`); err != nil {
					t.Fatal(err)
				}
				if db, err := OpenRuntime(ctx, path); err == nil {
					_ = db.Close()
					t.Fatal("runtime opener accepted old/mixed format")
				}
				var payload []byte
				if err := raw.QueryRow(`SELECT payload FROM ` + table).Scan(&payload); err != nil || !bytes.Equal(payload, []byte{0, 255}) {
					t.Fatalf("existing data changed: %x %v", payload, err)
				}
				var initialized int
				if err := raw.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='runtime_schema'`).Scan(&initialized); err != nil || (initialized == 1) != mixed {
					t.Fatalf("old format silently initialized: %d %v", initialized, err)
				}
			})
		}
	}
}

func TestRuntimeDatabaseRefusesDamagedSchema(t *testing.T) {
	for _, damage := range []string{
		`DROP TABLE ate_pending_requests`,
		`ALTER TABLE agent_checkpoints ADD COLUMN unexpected TEXT`,
		`UPDATE runtime_schema SET checksum='wrong'`,
		`INSERT INTO runtime_schema VALUES(2,'extra')`,
		`DELETE FROM runtime_schema`,
		`CREATE VIEW business_view AS SELECT * FROM agent_checkpoints`,
	} {
		t.Run(damage, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "runtime.db")
			db, err := OpenRuntime(t.Context(), path)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Checkpoints().Set(t.Context(), "keep", []byte("checkpoint")); err != nil {
				t.Fatal(err)
			}
			if _, err := db.db.Exec(damage); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if reopened, err := OpenRuntime(t.Context(), path); err == nil {
				_ = reopened.Close()
				t.Fatal("damaged runtime schema accepted")
			}
			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			var value []byte
			if err := raw.QueryRow(`SELECT checkpoint FROM agent_checkpoints WHERE checkpoint_id='keep'`).Scan(&value); err != nil || string(value) != "checkpoint" {
				t.Fatalf("failed open changed checkpoint: %q %v", value, err)
			}
		})
	}
}

func TestRuntimeCheckpointIsolationAndCorruption(t *testing.T) {
	db := newRetentionRuntime(t)
	other := newRetentionRuntime(t)
	ctx := t.Context()
	for _, invalid := range []struct {
		id   string
		data []byte
	}{{"", []byte("data")}, {"key", nil}} {
		if err := db.Checkpoints().Set(ctx, invalid.id, invalid.data); err == nil {
			t.Fatal("invalid checkpoint accepted")
		}
	}
	input := []byte("checkpoint")
	if err := db.Checkpoints().Set(ctx, "key", input); err != nil {
		t.Fatal(err)
	}
	input[0] = 'X'
	got, found, err := db.Checkpoints().Get(ctx, "key")
	if err != nil || !found || string(got) != "checkpoint" {
		t.Fatalf("saved bytes changed: %q %v", got, err)
	}
	got[0] = 'Y'
	if _, found, err := other.Checkpoints().Get(ctx, "key"); err != nil || found {
		t.Fatalf("checkpoint leaked across runtimes: %v %v", found, err)
	}
	if _, err := db.db.Exec(`UPDATE agent_checkpoints SET checksum='corrupt'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.Checkpoints().Get(ctx, "key"); err == nil {
		t.Fatal("corrupt checkpoint accepted")
	}
	for range 2 {
		if err := db.Checkpoints().Delete(ctx, "key"); err != nil {
			t.Fatal(err)
		}
	}
	if _, found, err := db.Checkpoints().Get(ctx, "key"); err != nil || found {
		t.Fatalf("deleted checkpoint remains: %v %v", found, err)
	}
}

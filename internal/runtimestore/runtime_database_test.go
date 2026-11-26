package runtimestore

import (
	"bytes"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

// Frozen bytes of d0ff9dd's runtimeDDL + ateRequestsDDL + ateRequestRejectionsDDL.
//
//go:embed testdata/runtime_v1.sql
var releasedRuntimeDDL string

// releasedRuntimeSchema derives v2 from the frozen v1 fixture and checks the
// result against its independently recorded release checksum.
func releasedRuntimeSchema(version int) (string, string, string) {
	if version == 1 {
		return releasedRuntimeDDL, runtimeV1Checksum, runtimeV1Layout
	}
	var ddl strings.Builder
	for part := range strings.SplitAfterSeq(releasedRuntimeDDL, ";\n") {
		statement := strings.TrimSpace(part)
		if !strings.HasPrefix(statement, "CREATE TABLE plugin_") && !strings.HasPrefix(statement, "CREATE UNIQUE INDEX plugin_") {
			ddl.WriteString(part)
		}
	}
	return ddl.String(), runtimeV2Checksum, runtimeV2Layout
}

func createReleasedRuntime(t *testing.T, version int) (*Database, string, string) {
	t.Helper()
	ddl, checksum, layout := releasedRuntimeSchema(version)
	if sha256Hex([]byte(ddl)) != checksum || runtimeLayoutFromDDL(ddl) != layout {
		t.Fatalf("released schema fixture drifted: version %d", version)
	}
	path := filepath.Join(t.TempDir(), "runtime-v3.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if _, err := raw.Exec(ddl); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO runtime_schema VALUES(?, ?)`, version, checksum); err != nil {
		t.Fatal(err)
	}
	return &Database{db: raw}, path, checksum
}

func TestRuntimeDatabaseReopensReleasedFormatWithoutChangingEvidence(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			testReopensReleasedFormat(t, version)
		})
	}
}

func testReopensReleasedFormat(t *testing.T, schemaVersion int) {
	t.Helper()
	ctx := t.Context()
	previous, path, schemaChecksum := createReleasedRuntime(t, schemaVersion)
	checkpoint := []byte("original checkpoint")
	if err := previous.Checkpoints().Set(ctx, "turn", checkpoint); err != nil {
		t.Fatal(err)
	}
	pending := atePendingRecord(t, "pending")
	if err := previous.ATERequests().BindAuthority(ctx, pending.StoreID); err != nil {
		t.Fatal(err)
	}
	if err := previous.ATERequests().Insert(ctx, pending); err != nil {
		t.Fatal(err)
	}
	if _, err := previous.ATERequests().BeginAttempt(ctx, pending); err != nil {
		t.Fatal(err)
	}
	manifest := runtimeManifest(t, "original")
	id, err := manifest.ID()
	if err != nil {
		t.Fatal(err)
	}
	ref := tool.SnapshotReference{Kind: "turn", Scope: "session", ID: "turn"}
	if err := previous.RuntimeSnapshots().WithReferenceGuard(allowSnapshotReference).Save(ctx, manifest, ref); err != nil {
		t.Fatal(err)
	}
	// Historical projections are opaque and must be left intact on open.
	archived := []byte("retained evidence")
	if _, err := previous.db.Exec(`INSERT INTO lsp_diagnostic_projections VALUES('workspace', 'file.go', 'revision', ?, '2026-01-01')`, archived); err != nil {
		t.Fatal(err)
	}
	if schemaVersion == 1 {
		if _, err := previous.db.Exec(`INSERT INTO plugin_blobs VALUES('retained', ?)`, archived); err != nil {
			t.Fatal(err)
		}
	}
	if err := previous.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		current, err := OpenRuntime(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = current.Close() })
		got, found, err := current.Checkpoints().Get(ctx, "turn")
		if err != nil || !found || !bytes.Equal(got, checkpoint) {
			t.Fatalf("checkpoint changed: %q %v", got, err)
		}
		request, found, err := current.ATERequests().Get(ctx, pending.Request.Key())
		if err != nil || !found || !request.Attempted || request.Digest != pending.Digest || !bytes.Equal(request.Request.Body, pending.Request.Body) {
			t.Fatalf("pending request lost identity/body/attempt: %+v %v", request, err)
		}
		if err := current.ATERequests().BindAuthority(ctx, "other-store"); !errors.Is(err, aterequest.ErrConflict) {
			t.Fatalf("original authority lost: %v", err)
		}
		restored, err := current.RuntimeSnapshots().Load(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if restoredID, err := restored.ID(); err != nil || restoredID != id {
			t.Fatalf("snapshot changed: %s %v", restoredID, err)
		}
		refs, err := current.RuntimeSnapshots().References(ctx, id)
		if err != nil || !slices.Equal(refs, []tool.SnapshotReference{ref}) {
			t.Fatalf("references changed: %+v %v", refs, err)
		}
		var version int
		var checksum string
		if err := current.db.QueryRow(`SELECT version, checksum FROM runtime_schema`).Scan(&version, &checksum); err != nil || version != schemaVersion || checksum != schemaChecksum {
			t.Fatalf("old schema relabeled: %d %s %v", version, checksum, err)
		}
		if err := current.db.QueryRow(`SELECT report FROM lsp_diagnostic_projections`).Scan(&got); err != nil || !bytes.Equal(got, archived) {
			t.Fatalf("retired evidence changed: %q %v", got, err)
		}
		if schemaVersion == 1 {
			if err := current.db.QueryRow(`SELECT source FROM plugin_blobs WHERE content_sha='retained'`).Scan(&got); err != nil || !bytes.Equal(got, archived) {
				t.Fatalf("retired blob changed: %q %v", got, err)
			}
		}
		if err := current.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRuntimeDatabaseRefusesDamagedReleasedSchema(t *testing.T) {
	for _, test := range []struct {
		version int
		damage  string
	}{
		{1, `DROP TABLE plugin_blobs`},
		{1, `ALTER TABLE plugin_revisions ADD COLUMN unexpected TEXT`},
		{1, `DROP TABLE ate_pending_requests`},
		{2, `DROP INDEX idx_lsp_diagnostics_latest`},
		{2, `ALTER TABLE lsp_diagnostic_projections ADD COLUMN unexpected TEXT`},
		{2, `UPDATE runtime_schema SET checksum='wrong'`},
	} {
		t.Run(fmt.Sprintf("v%d/%s", test.version, test.damage), func(t *testing.T) {
			previous, path, _ := createReleasedRuntime(t, test.version)
			if err := previous.Checkpoints().Set(t.Context(), "keep", []byte("original")); err != nil {
				t.Fatal(err)
			}
			if _, err := previous.db.Exec(test.damage); err != nil {
				t.Fatal(err)
			}
			if current, err := OpenRuntime(t.Context(), path); err == nil {
				_ = current.Close()
				t.Fatal("damaged released schema accepted")
			}
			got, found, err := previous.Checkpoints().Get(t.Context(), "keep")
			if err != nil || !found || string(got) != "original" {
				t.Fatalf("failed open changed evidence: %q %v", got, err)
			}
		})
	}
}

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
	var version, retiredObjects int
	if err := db.db.QueryRowContext(ctx, `SELECT version,(SELECT count(*) FROM sqlite_master
        WHERE name GLOB 'plugin_*' OR name IN ('lsp_diagnostic_projections', 'idx_lsp_diagnostics_latest')) FROM runtime_schema`).Scan(&version, &retiredObjects); err != nil {
		t.Fatal(err)
	}
	if version != 3 || retiredObjects != 0 {
		t.Fatalf("fresh runtime: version=%d retired objects=%d", version, retiredObjects)
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
		`INSERT INTO runtime_schema VALUES(4,'extra')`,
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

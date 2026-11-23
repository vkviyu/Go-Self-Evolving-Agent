package runtimestore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/plugin"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func catalogRecord(revision string, source []byte) plugin.RevisionRecord {
	sum := sha256.Sum256(source)
	return plugin.RevisionRecord{Version: plugin.CatalogVersion, RuntimeIdentity: "test-limits", ContentSHA256: hex.EncodeToString(sum[:]), Manifest: plugin.Manifest{
		PluginID: "test-plugin", Timeout: time.Second,
		Tool: tool.Descriptor{Name: "echo", Revision: revision, Description: "Test echo", InputSchema: []byte(`{"type":"object"}`), RiskClass: tool.RiskPure},
	}}
}

func TestPluginCatalogLifecycleSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "plugins.db")
	database, err := OpenRuntime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	catalog := database.Plugins()
	source := []byte("storage fixture; compilation belongs to manager")
	for _, revision := range []string{"v1", "v2", "v3"} {
		if err := catalog.Save(ctx, catalogRecord(revision, source), source); err != nil {
			t.Fatal(err)
		}
	}
	var blobs int
	if err := database.db.QueryRow(`SELECT COUNT(*) FROM plugin_blobs`).Scan(&blobs); err != nil || blobs != 1 {
		t.Fatalf("dedup: %d, %v", blobs, err)
	}
	if err := catalog.Save(ctx, catalogRecord("v1", source), source); !errors.Is(err, plugin.ErrArtifactExists) {
		t.Fatalf("reused revision: %v", err)
	}
	foreign := catalogRecord("v4", source)
	foreign.Manifest.PluginID = "foreign"
	if err := catalog.Save(ctx, foreign, source); !errors.Is(err, plugin.ErrToolOwned) {
		t.Fatalf("cross-plugin takeover: %v", err)
	}
	if err := catalog.SetActive(ctx, "echo", "", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SetActive(ctx, "echo", "", "v2"); !errors.Is(err, plugin.ErrCatalogConflict) {
		t.Fatalf("stale CAS: %v", err)
	}
	if err := catalog.SetActive(ctx, "echo", "v1", "missing"); err == nil {
		t.Fatal("accepted missing revision")
	}
	assertCatalogStates(t, catalog, plugin.StateActive, plugin.StateStaged, plugin.StateStaged)
	if _, err := database.db.Exec(`UPDATE plugin_revisions SET state = 'active' WHERE revision = 'v2'`); err == nil {
		t.Fatal("allowed two active revisions")
	}
	if err := catalog.SetActive(ctx, "echo", "v1", "v2"); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = OpenRuntime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	catalog = database.Plugins()
	assertCatalogStates(t, catalog, plugin.StateRetired, plugin.StateActive, plugin.StateStaged)
	if err := catalog.SetActive(ctx, "echo", "v2", ""); err != nil {
		t.Fatal(err)
	}
	assertCatalogStates(t, catalog, plugin.StateRetired, plugin.StateRetired, plugin.StateStaged)
	if err := catalog.SetActive(ctx, "echo", "", "v1"); err != nil {
		t.Fatal(err)
	}
	assertCatalogStates(t, catalog, plugin.StateActive, plugin.StateRetired, plugin.StateStaged)
}

func assertCatalogStates(t *testing.T, catalog *PluginCatalog, states ...plugin.State) {
	t.Helper()
	items, err := catalog.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != len(states) {
		t.Fatalf("items = %d, want %d", len(items), len(states))
	}
	for i, state := range states {
		if items[i].State != state {
			t.Fatalf("revision %s = %s, want %s", items[i].Record.Manifest.Tool.Revision, items[i].State, state)
		}
	}
}

func TestPluginCatalogCorruptionFailsClosed(t *testing.T) {
	for _, query := range []string{
		`UPDATE plugin_revisions SET record_sha = 'corrupt' WHERE revision = 'v2'`,
		`UPDATE plugin_revisions SET plugin_id = 'foreign' WHERE revision = 'v2'`,
		`UPDATE plugin_blobs SET source = x'00'`,
		`UPDATE plugin_revisions SET record = x'00' WHERE revision = 'v2'`,
	} {
		t.Run(query, func(t *testing.T) {
			database, err := OpenRuntime(context.Background(), ":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			catalog := database.Plugins()
			source := []byte("source")
			for _, revision := range []string{"v1", "v2"} {
				if err := catalog.Save(context.Background(), catalogRecord(revision, source), source); err != nil {
					t.Fatal(err)
				}
			}
			if err := catalog.SetActive(context.Background(), "echo", "", "v1"); err != nil {
				t.Fatal(err)
			}
			if _, err := database.db.Exec(query); err != nil {
				t.Fatal(err)
			}
			if _, err := catalog.Load(context.Background()); err == nil {
				t.Fatal("loaded corrupt catalog")
			}
			if err := catalog.SetActive(context.Background(), "echo", "v1", "v2"); err == nil {
				t.Fatal("activated corrupt target")
			}
			var active string
			if err := database.db.QueryRow(`SELECT revision FROM plugin_revisions WHERE state = 'active'`).Scan(&active); err != nil || active != "v1" {
				t.Fatalf("changed active revision: %s, %v", active, err)
			}
		})
	}
}

func TestPluginCatalogFailedSaveLeavesNoBlob(t *testing.T) {
	database, err := OpenRuntime(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	// Fail after blob insertion to exercise transaction rollback, not just input validation.
	if _, err := database.db.Exec(`CREATE TRIGGER fail_plugin_save BEFORE INSERT ON plugin_revisions BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	source := []byte("source")
	if err := database.Plugins().Save(context.Background(), catalogRecord("v1", source), source); err == nil {
		t.Fatal("save unexpectedly succeeded")
	}
	for _, table := range []string{"plugin_blobs", "plugin_revisions"} {
		var count int
		if err := database.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("orphaned %s: %d, %v", table, count, err)
		}
	}
}

package sqlitestore

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func runtimeManifest(t *testing.T, revision string) tool.SnapshotManifest {
	t.Helper()
	registry := tool.NewRegistry()
	_, err := registry.Activate(tool.Binding{Descriptor: tool.Descriptor{Name: "echo", Revision: revision, Description: "Echo", InputSchema: []byte(`{"type":"object"}`), RiskClass: tool.RiskPure}, Executor: tool.InvokeFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) { return input, nil })})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := registry.Snapshot().Manifest()
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func TestRuntimeSnapshotsPersistReferencesWithoutRebinding(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "snapshots.db")
	database, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	manifest := runtimeManifest(t, "v1")
	id, err := manifest.ID()
	if err != nil {
		t.Fatal(err)
	}
	reference := tool.SnapshotReference{Kind: "turn", Scope: "session", ID: "turn"}
	for range 2 {
		if err := database.RuntimeSnapshots().Save(ctx, manifest, reference); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.RuntimeSnapshots().Save(ctx, manifest, tool.SnapshotReference{Kind: "flow", Scope: "team", ID: "key"}); err != nil {
		t.Fatal(err)
	}
	changed := runtimeManifest(t, "v2")
	if err := database.RuntimeSnapshots().Save(ctx, changed, reference); !errors.Is(err, tool.ErrSnapshotReferenceConflict) {
		t.Fatalf("rebound reference: %v", err)
	}
	changedID, _ := changed.ID()
	if _, err := database.RuntimeSnapshots().Load(ctx, changedID); !errors.Is(err, tool.ErrSnapshotUnavailable) {
		t.Fatalf("failed reference left orphan manifest: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := database.RuntimeSnapshots().Load(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if restoredID, err := restored.ID(); err != nil || restoredID != id {
		t.Fatalf("restored=%s, %v", restoredID, err)
	}
	refs, err := database.RuntimeSnapshots().References(ctx, id)
	if err != nil || len(refs) != 2 {
		t.Fatalf("refs=%+v, %v", refs, err)
	}
	if _, err := database.db.Exec(`UPDATE runtime_snapshots SET manifest = ? WHERE snapshot_id = ?`, []byte(`{"Version":2}`), id); err != nil {
		t.Fatal(err)
	}
	if _, err := database.RuntimeSnapshots().Load(ctx, id); !errors.Is(err, tool.ErrSnapshotUnavailable) {
		t.Fatalf("loaded corrupt snapshot: %v", err)
	}
	if err := database.RuntimeSnapshots().Save(ctx, manifest, reference); err == nil {
		t.Fatal("silently repaired corrupt immutable manifest")
	}
}

func TestRuntimeSnapshotAdmissionRollbackAndCancellation(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.db.Exec(`CREATE TRIGGER fail_snapshot_ref BEFORE INSERT ON runtime_snapshot_refs BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	manifest := runtimeManifest(t, "v1")
	reference := tool.SnapshotReference{Kind: "turn", Scope: "session", ID: "turn"}
	if err := database.RuntimeSnapshots().Save(ctx, manifest, reference); err == nil {
		t.Fatal("ignored reference save failure")
	}
	id, _ := manifest.ID()
	if _, err := database.RuntimeSnapshots().Load(ctx, id); !errors.Is(err, tool.ErrSnapshotUnavailable) {
		t.Fatalf("manifest survived rollback: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := database.RuntimeSnapshots().Save(canceled, manifest, reference); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled admission: %v", err)
	}
}

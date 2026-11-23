package runtimestore

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/retention"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func allowSnapshotReference(ctx context.Context, _ tool.SnapshotReference) error { return ctx.Err() }

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
	database, err := OpenRuntime(ctx, path)
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
		if err := database.RuntimeSnapshots().WithReferenceGuard(allowSnapshotReference).Save(ctx, manifest, reference); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.RuntimeSnapshots().WithReferenceGuard(allowSnapshotReference).Save(ctx, manifest, tool.SnapshotReference{Kind: "flow", Scope: "team", ID: "key"}); err != nil {
		t.Fatal(err)
	}
	changed := runtimeManifest(t, "v2")
	if err := database.RuntimeSnapshots().WithReferenceGuard(allowSnapshotReference).Save(ctx, changed, reference); !errors.Is(err, tool.ErrSnapshotReferenceConflict) {
		t.Fatalf("rebound reference: %v", err)
	}
	changedID, _ := changed.ID()
	if _, err := database.RuntimeSnapshots().Load(ctx, changedID); !errors.Is(err, tool.ErrSnapshotUnavailable) {
		t.Fatalf("failed reference left orphan manifest: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = OpenRuntime(ctx, path)
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
	if err := database.RuntimeSnapshots().WithReferenceGuard(allowSnapshotReference).Save(ctx, manifest, reference); err == nil {
		t.Fatal("silently repaired corrupt immutable manifest")
	}
}

func TestRuntimeSnapshotAdmissionRollbackAndCancellation(t *testing.T) {
	ctx := context.Background()
	database, err := OpenRuntime(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.db.Exec(`CREATE TRIGGER fail_snapshot_ref BEFORE INSERT ON runtime_snapshot_refs BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	manifest := runtimeManifest(t, "v1")
	reference := tool.SnapshotReference{Kind: "turn", Scope: "session", ID: "turn"}
	if err := database.RuntimeSnapshots().WithReferenceGuard(allowSnapshotReference).Save(ctx, manifest, reference); err == nil {
		t.Fatal("ignored reference save failure")
	}
	id, _ := manifest.ID()
	if _, err := database.RuntimeSnapshots().Load(ctx, id); !errors.Is(err, tool.ErrSnapshotUnavailable) {
		t.Fatalf("manifest survived rollback: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := database.RuntimeSnapshots().WithReferenceGuard(allowSnapshotReference).Save(canceled, manifest, reference); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled admission: %v", err)
	}
}

func TestRuntimeSnapshotAuthorityGuardAndAbandonCleanup(t *testing.T) {
	ctx := t.Context()
	db, err := OpenRuntime(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	manifest := runtimeManifest(t, "v1")
	id, _ := manifest.ID()
	ref := tool.SnapshotReference{Kind: "turn", Scope: "root", ID: "turn"}
	if err := db.RuntimeSnapshots().Save(ctx, manifest, ref); !errors.Is(err, ErrSnapshotAuthorityUnavailable) {
		t.Fatalf("unguarded save: %v", err)
	}
	failure := errors.New("authority unavailable")
	guarded := db.RuntimeSnapshots().WithReferenceGuard(func(context.Context, tool.SnapshotReference) error { return failure })
	if err := guarded.Save(ctx, manifest, ref); !errors.Is(err, failure) {
		t.Fatalf("guard error lost: %v", err)
	}
	if _, err := guarded.Load(ctx, id); !errors.Is(err, tool.ErrSnapshotUnavailable) {
		t.Fatalf("failed guard wrote manifest: %v", err)
	}
	guarded = db.RuntimeSnapshots().WithReferenceGuard(allowSnapshotReference)
	if err := guarded.Save(ctx, manifest, ref); err != nil {
		t.Fatal(err)
	}
	receipt := session.Abandonment{SessionID: "root", TurnIDs: []string{"turn", "not-yet-saved"}, Reason: "reviewed unknown external effects", AbandonedAt: time.Now().UTC()}
	for range 2 {
		n, err := guarded.CloseAbandonedReferences(ctx, receipt)
		if err != nil || n != 1 {
			t.Fatalf("cleanup replay=%d, %v", n, err)
		}
	}
	for _, turn := range receipt.TurnIDs {
		ref.ID = turn
		if err := guarded.Save(ctx, manifest, ref); !errors.Is(err, tool.ErrSnapshotReferenceReleased) {
			t.Fatalf("reopened %s: %v", turn, err)
		}
	}
	if _, err := guarded.Load(ctx, id); err != nil {
		t.Fatal("immutable manifest lost", err)
	}
	history, err := guarded.ReferenceHistory(ctx, id)
	if err != nil || len(history) != 1 || history[0].ReleasedAt == nil || history[0].ReleaseReason != receipt.Reason {
		t.Fatalf("release evidence=%+v %v", history, err)
	}
	receipt.Reason = "changed"
	if _, err := guarded.CloseAbandonedReferences(ctx, receipt); err == nil {
		t.Fatal("changed receipt replaced cleanup identity")
	}
}

func TestRuntimeSnapshotCleanupRollback(t *testing.T) {
	ctx := t.Context()
	db, err := OpenRuntime(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := db.RuntimeSnapshots().WithReferenceGuard(allowSnapshotReference)
	manifest := runtimeManifest(t, "v1")
	if err := store.Save(ctx, manifest, tool.SnapshotReference{Kind: "turn", Scope: "root", ID: "one"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`CREATE TRIGGER fail_close BEFORE INSERT ON runtime_consumer_closures WHEN NEW.consumer_id='two' BEGIN SELECT RAISE(ABORT,'injected cleanup failure'); END`); err != nil {
		t.Fatal(err)
	}
	receipt := session.Abandonment{SessionID: "root", TurnIDs: []string{"one", "two"}, Reason: "reviewed", AbandonedAt: time.Now().UTC()}
	if _, err := store.CloseAbandonedReferences(ctx, receipt); err == nil {
		t.Fatal("injected cleanup succeeded")
	}
	id, _ := manifest.ID()
	history, err := store.ReferenceHistory(ctx, id)
	if err != nil || len(history) != 1 || history[0].ReleasedAt != nil {
		t.Fatalf("partial cleanup: %+v %v", history, err)
	}
	if _, err := db.db.Exec(`DROP TRIGGER fail_close`); err != nil {
		t.Fatal(err)
	}
	if n, err := store.CloseAbandonedReferences(ctx, receipt); err != nil || n != 1 {
		t.Fatalf("cleanup retry: %d %v", n, err)
	}
}

func TestRuntimeSnapshotArchiveCleanupRollbackAndRetry(t *testing.T) {
	ctx := t.Context()
	db, err := OpenRuntime(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := db.RuntimeSnapshots().WithReferenceGuard(allowSnapshotReference)
	manifest := runtimeManifest(t, "v1")
	id, _ := manifest.ID()
	for _, turn := range []string{"one", "two"} {
		if err := store.Save(ctx, manifest, tool.SnapshotReference{Kind: "turn", Scope: "root", ID: turn}); err != nil {
			t.Fatal(err)
		}
	}
	consumers, err := db.RuntimeRetentionReferences(ctx, "root")
	if err != nil || len(consumers) != 2 {
		t.Fatalf("runtime references=%+v %v", consumers, err)
	}
	plan := retention.Plan{SessionID: "root", Sequence: 4, Consumers: consumers}
	plan.ID = plan.Digest()
	receipt := retention.Receipt{Plan: plan, ClosedAt: time.Now().UTC()}
	if _, err := db.db.Exec(`CREATE TRIGGER fail_archive BEFORE UPDATE ON runtime_snapshot_refs WHEN NEW.consumer_id='two' BEGIN SELECT RAISE(ABORT,'injected cleanup failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CleanupRetention(ctx, receipt); err == nil {
		t.Fatal("injected cleanup failure ignored")
	}
	if refs, err := store.References(ctx, id); err != nil || len(refs) != 2 {
		t.Fatalf("partial release survived rollback: %+v %v", refs, err)
	}
	if _, err := db.db.Exec(`DROP TRIGGER fail_archive`); err != nil {
		t.Fatal(err)
	}
	receipt.Plan.Consumers[1].SnapshotID = "wrong"
	receipt.Plan.ID = receipt.Plan.Digest()
	if _, err := db.CleanupRetention(ctx, receipt); err == nil {
		t.Fatal("snapshot identity mismatch accepted")
	}
	receipt.Plan.Consumers[1].SnapshotID = id
	receipt.Plan.ID = receipt.Plan.Digest()
	for range 2 {
		if n, err := db.CleanupRetention(ctx, receipt); err != nil || n != 2 {
			t.Fatalf("archive cleanup retry=%d %v", n, err)
		}
	}
	if refs, err := store.References(ctx, id); err != nil || len(refs) != 0 {
		t.Fatalf("released consumers remain active: %+v %v", refs, err)
	}
	if history, err := store.ReferenceHistory(ctx, id); err != nil || len(history) != 2 || history[0].ReleasedAt == nil || history[1].ReleasedAt == nil {
		t.Fatalf("archive evidence=%+v %v", history, err)
	}
	if err := store.Save(ctx, manifest, consumers[0].Reference); !errors.Is(err, tool.ErrSnapshotReferenceReleased) {
		t.Fatalf("archived consumer reopened: %v", err)
	}
}

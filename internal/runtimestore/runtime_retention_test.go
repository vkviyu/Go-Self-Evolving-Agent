package runtimestore

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/retention"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func newRetentionRuntime(t *testing.T) *Database {
	t.Helper()
	db, err := OpenRuntime(t.Context(), filepath.Join(t.TempDir(), "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestRuntimeRetentionCleanupAtomicRetryAndNoBusinessProjection(t *testing.T) {
	db := newRetentionRuntime(t)
	ctx := t.Context()
	manifest := runtimeManifest(t, "v1")
	snapshot, _ := manifest.ID()
	refs := []tool.SnapshotReference{{Kind: "turn", Scope: "root", ID: "one"}, {Kind: "flow", Scope: "team", ID: "flow"}}
	receipt := retention.Receipt{Plan: retention.Plan{ID: "native-plan", SessionID: "root"}, ClosedAt: time.Now().UTC()}
	for _, ref := range refs {
		if err := db.RuntimeSnapshots().WithReferenceGuard(allowSnapshotReference).Save(ctx, manifest, ref); err != nil {
			t.Fatal(err)
		}
		receipt.Plan.Consumers = append(receipt.Plan.Consumers, retention.Consumer{Reference: ref, SnapshotID: snapshot})
	}
	// Resource absent here is legitimate: ATE closes the consumer globally,
	// while this runtime can only release rows it actually owns.
	receipt.Plan.Consumers = append(receipt.Plan.Consumers, retention.Consumer{Reference: tool.SnapshotReference{Kind: "turn", Scope: "root", ID: "remote"}})
	if _, err := db.db.Exec(`CREATE TRIGGER fail_runtime_release BEFORE UPDATE ON runtime_snapshot_refs WHEN NEW.kind='flow' BEGIN SELECT RAISE(ABORT,'injected cleanup failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CleanupRetention(ctx, receipt); err == nil {
		t.Fatal("injected release failure ignored")
	}
	active, err := db.RuntimeSnapshots().References(ctx, snapshot)
	if err != nil || len(active) != 2 {
		t.Fatalf("partial local commit: %+v %v", active, err)
	}
	if _, err := db.db.Exec(`DROP TRIGGER fail_runtime_release`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		count, err := db.CleanupRetention(ctx, receipt)
		if err != nil || count != 2 {
			t.Fatalf("cleanup retry: %d %v", count, err)
		}
	}
	if _, err := db.RuntimeSnapshots().Load(ctx, snapshot); err != nil {
		t.Fatal("manifest lost", err)
	}
	for _, ref := range refs {
		if err := db.RuntimeSnapshots().WithReferenceGuard(allowSnapshotReference).Save(ctx, manifest, ref); !errors.Is(err, tool.ErrSnapshotReferenceReleased) {
			t.Fatalf("released resource reopened: %v", err)
		}
	}
	var receipts, closures, business int
	if err := db.db.QueryRow(`SELECT (SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='maintenance_receipts'),(SELECT COUNT(*) FROM runtime_consumer_closures),(SELECT COUNT(*) FROM sqlite_master WHERE name IN ('session_events','goal_runs','flow_runs'))`).Scan(&receipts, &closures, &business); err != nil {
		t.Fatal(err)
	}
	if receipts != 0 || closures != 0 || business != 0 {
		t.Fatalf("persistent business projection: receipts=%d closures=%d tables=%d", receipts, closures, business)
	}
}

func TestRuntimeRetentionRejectsMismatchedSnapshotAndReceipt(t *testing.T) {
	db := newRetentionRuntime(t)
	ctx := t.Context()
	manifest := runtimeManifest(t, "v1")
	ref := tool.SnapshotReference{Kind: "turn", Scope: "root", ID: "turn"}
	if err := db.RuntimeSnapshots().WithReferenceGuard(allowSnapshotReference).Save(ctx, manifest, ref); err != nil {
		t.Fatal(err)
	}
	receipt := retention.Receipt{Plan: retention.Plan{ID: "plan", SessionID: "root", Consumers: []retention.Consumer{{Reference: ref, SnapshotID: "different"}}}, ClosedAt: time.Now().UTC()}
	if _, err := db.CleanupRetention(ctx, receipt); !errors.Is(err, tool.ErrSnapshotReferenceConflict) {
		t.Fatalf("mismatch accepted: %v", err)
	}
	receipt.Plan.Consumers[0].SnapshotID, _ = manifest.ID()
	if _, err := db.CleanupRetention(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	receipt.Plan.ID = "other-plan"
	if _, err := db.CleanupRetention(ctx, receipt); err == nil {
		t.Fatal("different closure identity accepted")
	}
}

package runtimestore

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/plugin"
	"github.com/vkviyu/go-self-evolving-agent/internal/retention"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func TestRuntimeCollectionLocalBoundaryHistoryAndRetry(t *testing.T) {
	ctx := t.Context()
	local, remote := newRetentionRuntime(t), newRetentionRuntime(t)
	source := []byte("retained artifact source")
	record := catalogRecord("v1", source)
	manifest := runtimeManifest(t, "v1")
	snapshot, _ := manifest.ID()
	ref := tool.SnapshotReference{Kind: "turn", Scope: "root", ID: "turn"}
	for _, db := range []*Database{local, remote} {
		if err := db.Plugins().Save(ctx, record, source); err != nil {
			t.Fatal(err)
		}
		if err := db.RuntimeSnapshots().WithReferenceGuard(allowSnapshotReference).Save(ctx, manifest, ref); err != nil {
			t.Fatal(err)
		}
	}
	held, err := local.RuntimePluginCollectionPlan(ctx, "echo", "v1")
	if err != nil || len(held.Blockers) == 0 {
		t.Fatalf("retained whole toolset not protected: %+v %v", held, err)
	}
	archive := retention.Receipt{Plan: retention.Plan{ID: "native-archive", SessionID: "root", Consumers: []retention.Consumer{{Reference: ref, SnapshotID: snapshot}}}, ClosedAt: time.Now().UTC()}
	if _, err := local.CleanupRetention(ctx, archive); err != nil {
		t.Fatal(err)
	}
	plan, err := local.RuntimePluginCollectionPlan(ctx, "echo", "v1")
	if err != nil || len(plan.Blockers) != 0 || plan.ReclaimableBytes != len(source) {
		t.Fatalf("local plan: %+v %v", plan, err)
	}
	receipt := retention.CollectionReceipt{Plan: plan, CollectedAt: time.Now().UTC()}
	first, err := local.CleanupPluginCollection(ctx, receipt)
	if err != nil || !first.BlobDeleted {
		t.Fatalf("collection: %+v %v", first, err)
	}
	second, err := local.CleanupPluginCollection(ctx, receipt)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("retry: %+v %v", second, err)
	}
	if err := local.Plugins().Save(ctx, record, source); !errors.Is(err, plugin.ErrArtifactExists) {
		t.Fatalf("collected identity reused: %v", err)
	}
	if _, err := local.RuntimeSnapshots().Load(ctx, snapshot); err != nil {
		t.Fatal("manifest history lost", err)
	}
	remotePlan, err := remote.RuntimePluginCollectionPlan(ctx, "echo", "v1")
	if err != nil || len(remotePlan.Blockers) == 0 {
		t.Fatalf("foreign runtime resource affected: %+v %v", remotePlan, err)
	}
	items, err := remote.Plugins().Load(ctx)
	if err != nil || len(items) != 1 || string(items[0].Source) != string(source) {
		t.Fatalf("foreign artifact changed: %+v %v", items, err)
	}
	var receipts int
	if err := local.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='maintenance_receipts'`).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatalf("local business receipt: %d %v", receipts, err)
	}
}

func TestRuntimeCollectionSharedBlobChangedPlanAndFailures(t *testing.T) {
	for _, mode := range []string{"shared", "active", "changed", "corrupt-manifest", "cleanup-failure"} {
		t.Run(mode, func(t *testing.T) {
			db := newRetentionRuntime(t)
			ctx := t.Context()
			source := []byte("shared artifact")
			if err := db.Plugins().Save(ctx, catalogRecord("v1", source), source); err != nil {
				t.Fatal(err)
			}
			if mode == "shared" {
				if err := db.Plugins().Save(ctx, catalogRecord("v2", source), source); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "active" {
				if err := db.Plugins().SetActive(ctx, "echo", "", "v1"); err != nil {
					t.Fatal(err)
				}
			}
			plan, err := db.RuntimePluginCollectionPlan(ctx, "echo", "v1")
			if err != nil {
				t.Fatal(err)
			}
			if mode == "active" && len(plan.Blockers) == 0 {
				t.Fatal("active artifact collectable")
			}
			if mode == "changed" {
				if err := db.Plugins().SetActive(ctx, "echo", "", "v1"); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "corrupt-manifest" {
				if _, err := db.db.Exec(`INSERT INTO runtime_snapshots(snapshot_id,manifest) VALUES('invalid','{}')`); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "cleanup-failure" {
				if _, err := db.db.Exec(`CREATE TRIGGER fail_blob_delete BEFORE DELETE ON plugin_blobs BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
					t.Fatal(err)
				}
			}
			receipt := retention.CollectionReceipt{Plan: plan, CollectedAt: time.Now().UTC()}
			result, err := db.CleanupPluginCollection(ctx, receipt)
			if mode == "shared" {
				if err != nil || result.BlobDeleted {
					t.Fatalf("shared blob deleted: %+v %v", result, err)
				}
			} else {
				if err == nil {
					t.Fatal("unsafe collection accepted")
				}
				if mode == "changed" && !errors.Is(err, retention.ErrPlanChanged) {
					t.Fatalf("stale plan: %v", err)
				}
				var revisions, tombstones int
				if err := db.db.QueryRow(`SELECT (SELECT COUNT(*) FROM plugin_revisions),(SELECT COUNT(*) FROM plugin_revision_tombstones)`).Scan(&revisions, &tombstones); err != nil {
					t.Fatal(err)
				}
				if revisions != 1 || tombstones != 0 {
					t.Fatalf("failed cleanup committed: %d %d", revisions, tombstones)
				}
				if mode == "cleanup-failure" {
					if _, err := db.db.Exec(`DROP TRIGGER fail_blob_delete`); err != nil {
						t.Fatal(err)
					}
					if result, err := db.CleanupPluginCollection(ctx, receipt); err != nil || !result.BlobDeleted {
						t.Fatalf("cleanup retry: %+v %v", result, err)
					}
				}
			}
		})
	}
}

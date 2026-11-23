package runtimestore_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	ate "github.com/vkviyu/agent-temporal-engine"
	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/agent-temporal-engine/spacecatalog"
	"github.com/vkviyu/go-self-evolving-agent/internal/app"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateprocess"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/plugin"
	"github.com/vkviyu/go-self-evolving-agent/internal/retention"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimeowner"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	sessionate "github.com/vkviyu/go-self-evolving-agent/internal/session/atestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

// All paths and the native binary are temporary. No App/model/Supervisor or
// workspace/Git service is constructed to exercise the offline public API.
func TestRuntimeMaintenanceAppNativeArchiveCollectionAndSharedLock(t *testing.T) {
	ctx := t.Context()
	binary := filepath.Join(t.TempDir(), "ate")
	if output, err := exec.CommandContext(ctx, "go", "build", "-o", binary, "github.com/vkviyu/agent-temporal-engine/cmd/ate").CombinedOutput(); err != nil {
		t.Fatalf("build ATE: %v\n%s", err, output)
	}
	directory := filepath.Join(t.TempDir(), "ate")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	engine, err := ate.Create(directory, ate.Options{AgentProfile: true, EventProfile: true})
	if err != nil {
		t.Fatal(err)
	}
	created, err := engine.ApplySpace(ctx, api.ApplySpaceRequest{Key: model.RequestKey{Space: "gsea-authority", Principal: "gsea", RequestID: "test-create-authority"}, Command: spacecatalog.Apply{Name: "Test authority"}})
	if err != nil || created.Code != api.CodeDurableAck {
		_ = engine.Close()
		t.Fatalf("space: %+v %v", created, err)
	}
	snapshot, err := engine.Snapshot(ctx)
	if err != nil {
		_ = engine.Close()
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	cfg, err := (config.Config{HomeDirectory: t.TempDir(), DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir(), ATE: config.ATEConfig{Enabled: true, Binary: binary, Directory: directory, Timeout: 30 * time.Second}}).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	db, err := runtimestore.OpenRuntime(ctx, cfg.RuntimeDatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	process, err := ateprocess.New(ateprocess.Options{Binary: binary, Directory: directory, Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer process.Close()
	events, err := sessionate.New(sessionate.Options{Process: process, Pending: db.ATERequests(), Space: "gsea-authority", StoreID: snapshot.Fence.Store})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := tool.NewRegistry().Snapshot().Manifest()
	if err != nil {
		t.Fatal(err)
	}
	snapshotID, err := manifest.ID()
	if err != nil {
		t.Fatal(err)
	}
	ref := tool.SnapshotReference{Kind: "turn", Scope: "root", ID: "turn"}
	if err := db.RuntimeSnapshots().WithReferenceGuard(func(ctx context.Context, _ tool.SnapshotReference) error { return ctx.Err() }).Save(ctx, manifest, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := events.Append(ctx, "root", 0,
		session.PendingEvent{Type: session.EventTurnStarted, CorrelationID: "turn", RuntimeSnapshotID: snapshotID, Payload: []byte(`{}`)},
		session.PendingEvent{Type: session.EventTurnCompleted, CorrelationID: "turn", RuntimeSnapshotID: snapshotID, Payload: []byte(`{"content":"retained history"}`)},
	); err != nil {
		t.Fatal(err)
	}
	source := []byte("test resource; never executed")
	digest := sha256.Sum256(source)
	record := plugin.RevisionRecord{Version: plugin.CatalogVersion, RuntimeIdentity: "test", ContentSHA256: hex.EncodeToString(digest[:]), Manifest: plugin.Manifest{
		PluginID: "offline-plugin", Timeout: time.Second, Tool: tool.Descriptor{Name: "echo", Revision: "v1", Description: "Echo", RiskClass: tool.RiskPure, InputSchema: []byte(`{"type":"object"}`)},
	}}
	if err := db.Plugins().Save(ctx, record, source); err != nil {
		t.Fatal(err)
	}
	if err := process.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Match app/authority_scope.go exactly, with a DIFFERENT local runtime DB.
	canonical, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(string(snapshot.Fence.Store) + "\x00root"))
	lock, err := runtimeowner.Acquire(filepath.Join(filepath.Dir(canonical), ".gsea-runtime-owners", hex.EncodeToString(sum[:])))
	if err != nil {
		t.Fatal(err)
	}
	other := cfg
	other.DataDirectory = t.TempDir()
	_, lockErr := app.PreviewRetention(ctx, other, "root")
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(lockErr, runtimeowner.ErrOwned) {
		t.Fatalf("shared execution lock bypassed: %v", lockErr)
	}
	plan, err := app.PreviewRetention(ctx, cfg, "root")
	if err != nil || len(plan.Blockers) != 0 || len(plan.Consumers) != 1 {
		t.Fatalf("preview: %+v %v", plan, err)
	}
	if _, err := app.ArchiveSession(ctx, cfg, "root", plan.ID, false); err == nil {
		t.Fatal("archive without confirmation")
	}
	if _, err := app.ArchiveSession(ctx, cfg, "root", "stale", true); !errors.Is(err, retention.ErrPlanChanged) {
		t.Fatalf("stale archive: %v", err)
	}
	first, err := app.ArchiveSession(ctx, cfg, "root", plan.ID, true)
	if err != nil || first.ReleasedReferences != 1 || first.EvidenceDeleted {
		t.Fatalf("archive: %+v %v", first, err)
	}
	second, err := app.ArchiveSession(ctx, cfg, "root", plan.ID, true)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("archive retry: %+v %v", second, err)
	}
	collect, err := app.PreviewPluginCollection(ctx, cfg, "echo", "v1")
	if err != nil || len(collect.Blockers) != 0 {
		t.Fatalf("collection preview: %+v %v", collect, err)
	}
	if _, err := app.CollectPlugin(ctx, cfg, "echo", "v1", collect.ID, false); err == nil {
		t.Fatal("collection without confirmation")
	}
	collected, err := app.CollectPlugin(ctx, cfg, "echo", "v1", collect.ID, true)
	if err != nil || !collected.BlobDeleted {
		t.Fatalf("collection: %+v %v", collected, err)
	}
	replayed, err := app.CollectPlugin(ctx, cfg, "echo", "v1", collect.ID, true)
	if err != nil || !reflect.DeepEqual(collected, replayed) {
		t.Fatalf("collection retry: %+v %v", replayed, err)
	}
	db, err = runtimestore.OpenRuntime(ctx, cfg.RuntimeDatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	process, err = ateprocess.New(ateprocess.Options{Binary: binary, Directory: directory, Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer process.Close()
	events, err = sessionate.New(sessionate.Options{Process: process, Pending: db.ATERequests(), Space: "gsea-authority", StoreID: snapshot.Fence.Store})
	if err != nil {
		t.Fatal(err)
	}
	history, err := events.Load(ctx, "root", 0, 10)
	if err != nil || len(history) != 3 || string(history[1].Payload) != `{"content":"retained history"}` {
		t.Fatalf("history lost: %+v %v", history, err)
	}
}

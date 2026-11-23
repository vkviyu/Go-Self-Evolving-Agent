package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimeowner"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	sessionate "github.com/vkviyu/go-self-evolving-agent/internal/session/atestore"
)

// Seed only storage, so these tests do not construct executable services even
// while preparing interrupted histories.
func newSessionReconcileFixture(t *testing.T) (config.Config, *App) {
	t.Helper()
	cfg, err := testConfig(t, config.Config{}).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	db, err := runtimestore.OpenRuntime(t.Context(), cfg.RuntimeDatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	resources, err := openAuthority(t.Context(), cfg, db.ATERequests())
	if err != nil {
		t.Fatal(err)
	}
	a := &App{config: cfg, database: db, authority: resources, temporalProcess: resources.process}
	t.Cleanup(func() { _ = a.Close() })
	if err := a.controlRepositories(db.ATERequests()); err != nil {
		t.Fatal(err)
	}
	return cfg, a
}

func saveSessionReconcilePlan(t *testing.T, a *App, id session.ID) (uint64, session.Event) {
	t.Helper()
	pending := &startupPending{ATERequestStore: a.database.ATERequests(), mode: "unattempted"}
	writer, err := sessionate.New(sessionate.Options{
		Process: a.authority.bound, Pending: pending, Space: authoritySpace, StoreID: a.authority.storeID,
	})
	if err != nil {
		t.Fatal(err)
	}
	sequence, err := writer.CurrentSequence(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	event := session.PendingEvent{Type: session.EventUserMessage, CorrelationID: "saved-turn",
		Payload: json.RawMessage(`{"text":"private evidence ` + strings.Repeat("x", 400<<10) + `"}`)}
	if _, err := writer.Append(t.Context(), id, sequence, event); err == nil {
		t.Fatal("expected interruption before upload")
	}
	if err := a.authority.bound.WithFreshClient(t.Context(), func(*client.Client) error { return nil }); err != nil {
		t.Fatalf("reopen authority after expected interruption: %v", err)
	}
	return sequence, pending.original
}

func TestSessionReconcileRecoversOnlyAuthoritativeScope(t *testing.T) {
	cfg, first := newSessionReconcileFixture(t)
	ctx := t.Context()
	root, _, err := first.repositories.EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "interactive-unregistered"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := first.repositories.CreateChild(ctx, orchestration.SpawnSpec{
		TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker-v1",
		InitialPrompt: "private child prompt", IdempotencyKey: "child",
	})
	if err != nil {
		t.Fatal(err)
	}
	child, err = first.repositories.TransitionAgent(ctx, orchestration.AgentTransition{
		AgentID: child.ID, Expected: child.Version, Target: orchestration.AgentRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	foreign, _, err := first.repositories.EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "foreign"})
	if err != nil {
		t.Fatal(err)
	}
	ids := []session.ID{root.SessionID, child.SessionID, foreign.SessionID}
	sequences := make(map[session.ID]uint64)
	originals := make(map[session.ID]session.Event)
	for _, id := range ids {
		sequences[id], originals[id] = saveSessionReconcilePlan(t, first, id)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	// Invalid executable settings must be irrelevant to this storage boundary.
	cfg.Model.Provider = "not-a-provider"
	cfg.LSP.Enabled, cfg.LSP.Command = true, filepath.Join(t.TempDir(), "must-not-start")
	for attempt := 0; attempt < 2; attempt++ {
		result, err := ReconcileSession(ctx, cfg, root.SessionID)
		if err != nil || result.SessionID != root.SessionID || result.Outcome != "reconciled" {
			t.Fatalf("reconcile attempt %d: %+v %v", attempt, result, err)
		}
	}
	reader, err := OpenControl(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	for _, id := range ids {
		history, err := reader.authority.sessions.Load(ctx, id, sequences[id], 10)
		if err != nil {
			t.Fatal(err)
		}
		if id == foreign.SessionID {
			if len(history) != 0 {
				t.Fatal("reconciled a foreign plan")
			}
		} else if len(history) != 1 || !reflect.DeepEqual(history[0], originals[id]) {
			t.Fatalf("saved identity or evidence changed for %s", id)
		}
	}
	unchanged, err := reader.repositories.GetAgent(ctx, child.ID)
	if err != nil || !reflect.DeepEqual(unchanged, child) {
		t.Fatalf("reconciliation started Supervisor recovery: %v", err)
	}
	db, err := runtimestore.OpenRuntime(ctx, cfg.RuntimeDatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	left, err := db.ATERequests().List(ctx, aterequest.ListOptions{Limit: 128, MaxBytes: aterequest.MaxListBytes})
	if err != nil || len(left) != 2 {
		t.Fatalf("expected untouched foreign upload/append, got %d: %v", len(left), err)
	}
	if _, err := os.Stat(cfg.SessionRegistryPath()); !os.IsNotExist(err) {
		t.Fatalf("reconcile opened the session catalog: %v", err)
	}
}

func TestSessionReconcileOwnershipExclusion(t *testing.T) {
	cfg, first := newSessionReconcileFixture(t)
	ctx := t.Context()
	root, _, err := first.repositories.EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "owned"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := first.repositories.CreateChild(ctx, orchestration.SpawnSpec{
		TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker-v1",
		InitialPrompt: "child", IdempotencyKey: "child",
	})
	if err != nil {
		t.Fatal(err)
	}
	sequence, _ := saveSessionReconcilePlan(t, first, root.SessionID)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	reconcileCfg := cfg
	reconcileCfg.ATE.Timeout = 100 * time.Millisecond
	for _, target := range []session.ID{root.SessionID, child.SessionID} {
		t.Run(string(target), func(t *testing.T) {
			other := cfg
			other.DataDirectory = t.TempDir()
			holder, err := OpenControl(ctx, other)
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Close()
			holder.stateMu.Lock()
			err = holder.ownSession(target)
			holder.stateMu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ReconcileSession(ctx, reconcileCfg, root.SessionID); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("bypassed %s owner in another data directory: %v", target, err)
			}
			current, err := holder.authority.sessions.CurrentSequence(ctx, root.SessionID)
			if err != nil || current != sequence {
				t.Fatalf("advanced root before acquiring all child locks: %d %v", current, err)
			}
		})
	}
	lock, err := runtimeowner.Acquire(cfg.RuntimeDatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReconcileSession(ctx, cfg, root.SessionID); !errors.Is(err, runtimeowner.ErrOwned) {
		t.Fatalf("bypassed runtime database owner: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	// Every failed attempt must release all leases it acquired.
	if _, err := ReconcileSession(ctx, cfg, root.SessionID); err != nil {
		t.Fatalf("failure leaked ownership: %v", err)
	}
}

func TestSessionReconcileRejectsMissingStoresBeforeInitialization(t *testing.T) {
	for _, mode := range []string{"missing-data", "missing-db", "empty-db", "empty-authority", "missing-workspace", "invalid-id", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			base := t.TempDir()
			cfg := config.Config{DataDirectory: filepath.Join(base, "data"), WorkspaceDirectory: filepath.Join(base, "workspace"),
				ATE: config.ATEConfig{Enabled: true, Binary: filepath.Join(base, "must-not-launch"), Directory: filepath.Join(base, "authority")}}
			for _, directory := range []string{cfg.DataDirectory, cfg.WorkspaceDirectory, cfg.ATE.Directory} {
				if err := os.Mkdir(directory, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			id := session.ID("root")
			switch mode {
			case "missing-data":
				cfg.DataDirectory = filepath.Join(base, "absent")
			case "empty-db":
				if err := os.WriteFile(cfg.RuntimeDatabasePath(), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			case "empty-authority":
				db, err := runtimestore.OpenRuntime(t.Context(), cfg.RuntimeDatabasePath())
				if err != nil {
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			case "missing-workspace":
				cfg.WorkspaceDirectory = filepath.Join(base, "absent")
			case "invalid-id":
				id = "../bad"
			case "disabled":
				cfg.ATE.Enabled = false
			}
			before, err := os.ReadDir(base)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ReconcileSession(t.Context(), cfg, id); err == nil {
				t.Fatal("accepted invalid recovery target")
			}
			after, err := os.ReadDir(base)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("invalid request created resources")
			}
			if mode != "empty-db" && mode != "empty-authority" {
				if _, err := os.Stat(cfg.RuntimeDatabasePath()); !os.IsNotExist(err) {
					t.Fatalf("initialized runtime database: %v", err)
				}
			}
			entries, err := os.ReadDir(cfg.ATE.Directory)
			if err != nil || len(entries) != 0 {
				t.Fatalf("initialized authority: %v", err)
			}
		})
	}
}

func TestSessionReconcileUnknownSessionReleasesOwnership(t *testing.T) {
	cfg, first := newSessionReconcileFixture(t)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		_, err := ReconcileSession(t.Context(), cfg, "missing")
		if err == nil || errors.Is(err, runtimeowner.ErrOwned) {
			t.Fatalf("unknown Session accepted or leaked ownership: %v", err)
		}
	}
}

func TestSessionReconcileHistoryWithoutRoot(t *testing.T) {
	cfg, first := newSessionReconcileFixture(t)
	id := session.ID("history-only")
	if _, err := first.authority.sessions.Append(t.Context(), id, 0, session.PendingEvent{
		Type: session.EventUserMessage, Payload: json.RawMessage(`{"text":"original"}`),
	}); err != nil {
		t.Fatal(err)
	}
	sequence, original := saveSessionReconcilePlan(t, first, id)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ReconcileSession(t.Context(), cfg, id); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenControl(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	history, err := reader.authority.sessions.Load(t.Context(), id, sequence, 10)
	if err != nil || len(history) != 1 || !reflect.DeepEqual(history[0], original) {
		t.Fatalf("history-only recovery failed: count=%d err=%v", len(history), err)
	}
	if _, err := reader.repositories.RootThreadBySession(t.Context(), id); !errors.Is(err, orchestration.ErrNotFound) {
		t.Fatalf("reconcile created an orchestration root: %v", err)
	}
}

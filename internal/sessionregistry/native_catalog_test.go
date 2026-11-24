package sessionregistry_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	ate "github.com/vkviyu/agent-temporal-engine"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/agent-temporal-engine/service"
	"github.com/vkviyu/agent-temporal-engine/transport"
	"github.com/vkviyu/go-self-evolving-agent/internal/app"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateprocess"
	"github.com/vkviyu/go-self-evolving-agent/internal/cli"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	sessionate "github.com/vkviyu/go-self-evolving-agent/internal/session/atestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/sessionregistry"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

const catalogHelperEnvironment = "GSEA_NATIVE_CATALOG_TEST"

// Exercise the real ATE service over authenticated subprocess stdio, including
// closing and reopening the native store between catalog operations.
func TestMain(m *testing.M) {
	if os.Getenv(catalogHelperEnvironment) == "1" && len(os.Args) > 2 &&
		(os.Args[1] == "init-state" || os.Args[1] == "serve") {
		if err := serveCatalogATE(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func serveCatalogATE() error {
	directory := os.Args[len(os.Args)-1]
	options := ate.Options{AgentProfile: true, EventProfile: true}
	if os.Args[1] == "init-state" {
		store, err := ate.Create(directory, options)
		if err != nil {
			return err
		}
		return store.Close()
	}
	store, report, err := ate.OpenExisting(directory, options)
	if err != nil {
		return err
	}
	defer store.Close()
	if !report.Verified {
		return errors.New("native recovery not verified")
	}
	stream, err := transport.NewPipes(os.Stdin, os.Stdout)
	if err != nil {
		return err
	}
	server, err := service.New(store, service.Options{
		Credentials: map[string]service.Credential{
			os.Getenv("GSEA_ATE_PRIVATE_LAUNCH_TOKEN"): {
				Principal: "gsea", AllSpaces: true, AllowGlobalSnapshot: true,
			},
		},
	})
	if err != nil {
		return err
	}
	_ = server.ServeStream(context.Background(), stream)
	return server.Shutdown(context.Background())
}

func catalogConfig(t *testing.T) config.Config {
	t.Helper()
	t.Setenv(catalogHelperEnvironment, "1")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home, workspace := t.TempDir(), t.TempDir()
	return config.Config{
		HomeDirectory: home, DataDirectory: home, WorkspaceDirectory: workspace,
		ATE: config.ATEConfig{Enabled: true, Binary: binary, Directory: filepath.Join(home, "temporal"), Timeout: 30 * time.Second},
	}
}

func openCatalog(t *testing.T, cfg config.Config) sessionregistry.Catalog {
	t.Helper()
	registry, err := app.OpenSessionRegistry(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := registry.Close(); err != nil {
			t.Error(err)
		}
	})
	return registry
}

func TestMissingAndUnavailableCatalogArePredictable(t *testing.T) {
	ctx := context.Background()
	registry := openCatalog(t, catalogConfig(t))
	if err := registry.UpdateState(ctx, "missing", sessionregistry.StateStopped, 0); !errors.Is(err, sessionregistry.ErrNotFound) {
		t.Fatal(err)
	}
	if err := registry.Remove(ctx, "missing"); !errors.Is(err, sessionregistry.ErrNotFound) {
		t.Fatal(err)
	}
	var unavailable *sessionregistry.Authority
	if _, err := unavailable.Get(ctx, "missing"); err == nil {
		t.Fatal("nil catalog accepted")
	}
	if _, err := unavailable.List(ctx); err == nil {
		t.Fatal("nil catalog accepted")
	}
}

func nativeCatalogStores(t *testing.T, cfg config.Config) (*ateauthority.Store, *sessionate.Store) {
	t.Helper()
	ctx := context.Background()
	// Initialize the same authority through the application entry point.
	if err := openCatalog(t, cfg).Close(); err != nil {
		t.Fatal(err)
	}
	pending, err := runtimestore.OpenRuntime(ctx, filepath.Join(cfg.HomeDirectory, "authority-runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	process, err := ateprocess.New(ateprocess.Options{Binary: cfg.ATE.Binary, Directory: cfg.ATE.Directory, Timeout: cfg.ATE.Timeout})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := errors.Join(process.Close(), pending.Close()); err != nil {
			t.Error(err)
		}
	})
	identities := make(chan model.StoreID, 1)
	if err := process.WithClient(ctx, func(c *client.Client) error {
		snapshot, err := c.Snapshot(ctx)
		if err == nil {
			identities <- snapshot.Fence.Store
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	records, err := ateauthority.New(ateauthority.Options{
		Process: process, Pending: pending.ATERequests(), Space: "gsea-authority", Principal: "gsea",
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := sessionate.New(sessionate.Options{
		Process: process, Pending: pending.ATERequests(), Space: "gsea-authority", StoreID: <-identities,
	})
	if err != nil {
		t.Fatal(err)
	}
	return records, events
}

func nativeCatalogItem(cfg config.Config, id string) sessionregistry.Session {
	runtime := filepath.Join(cfg.HomeDirectory, "sessions", id)
	return sessionregistry.Session{
		ID: id, Name: "A stable session", Workspace: cfg.WorkspaceDirectory, Runtime: runtime,
		DataDirectory: filepath.Join(runtime, "data"), TempDirectory: filepath.Join(runtime, "tmp"),
		ToolState: filepath.Join(runtime, "tool-state"), Model: "test-model", Provider: "test-provider",
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func TestNativeCatalogListShowReopenAndRemove(t *testing.T) {
	ctx := context.Background()
	cfg := catalogConfig(t)
	// Invalid legacy files must neither be read nor auto-migrated.
	for _, name := range []string{"control.db", "sessions.db"} {
		if err := os.WriteFile(filepath.Join(cfg.HomeDirectory, name), []byte("legacy data: preserve"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	registry := openCatalog(t, cfg)
	for _, id := range []string{"stable-b", "stable-a"} {
		if err := registry.Create(ctx, nativeCatalogItem(cfg, id)); err != nil {
			t.Fatal(err)
		}
	}
	items, err := registry.List(ctx)
	if err != nil || len(items) != 2 || items[0].ID != "stable-a" || items[1].ID != "stable-b" {
		t.Fatalf("catalog list: %+v, %v", items, err)
	}
	item, err := registry.Get(ctx, "stable-a")
	if err != nil || item.Name != "A stable session" || item.Model != "test-model" || item.State != sessionregistry.StateCreated {
		t.Fatalf("catalog show: %+v, %v", item, err)
	}
	for _, directory := range []string{item.Runtime, item.DataDirectory, item.TempDirectory, item.ToolState} {
		info, err := os.Stat(directory)
		if err != nil || !info.IsDir() {
			t.Fatalf("resource directory %q: %v", directory, err)
		}
	}
	duplicate := item
	duplicate.Runtime = filepath.Join(cfg.HomeDirectory, "duplicate-must-not-exist")
	if err := registry.Create(ctx, duplicate); !errors.Is(err, ateauthority.ErrExists) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := os.Stat(duplicate.Runtime); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("duplicate created resources: %v", err)
	}
	blocker := filepath.Join(cfg.HomeDirectory, "keep-file")
	if err := os.WriteFile(blocker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	invalid := nativeCatalogItem(cfg, "failed-resources")
	invalid.TempDirectory = filepath.Join(blocker, "invalid")
	if err := registry.Create(ctx, invalid); err == nil {
		t.Fatal("accepted invalid resource directory")
	}
	if _, err := registry.Get(ctx, invalid.ID); !errors.Is(err, sessionregistry.ErrNotFound) {
		t.Fatalf("registered session with invalid resources: %v", err)
	}
	if _, err := os.Stat(invalid.Runtime); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed registration left resources: %v", err)
	}
	if data, err := os.ReadFile(blocker); err != nil || string(data) != "keep" {
		t.Fatalf("failed registration changed user data: %q, %v", data, err)
	}
	if err := registry.UpdateState(ctx, item.ID, sessionregistry.StateRunning, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if err := registry.Remove(ctx, item.ID); err == nil {
		t.Fatal("removed running session")
	}
	if err := registry.UpdateState(ctx, item.ID, sessionregistry.StateCreated, 0); err == nil {
		t.Fatal("reset existing session to created")
	}
	if err := registry.UpdateState(ctx, item.ID, sessionregistry.StateStopped, 0); err != nil {
		t.Fatal(err)
	}
	saved, err := registry.Get(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	offline := cfg
	offline.WorkspaceDirectory = filepath.Join(cfg.HomeDirectory, "missing-workspace")
	offline.DataDirectory = filepath.Join(cfg.HomeDirectory, "sessions.db")
	registry = openCatalog(t, offline)
	reopened, err := registry.ReconcileStopped(ctx, item.ID)
	if err != nil || !reflect.DeepEqual(saved, reopened) {
		t.Fatalf("reopened session: %+v, %v; want %+v", reopened, err, saved)
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	records, events := nativeCatalogStores(t, cfg)
	if err := records.View(ctx, func(tx *ateauthority.Tx) error {
		count := 0
		err := tx.List("gsea/v1/session-catalog/", func(key string, raw json.RawMessage) error {
			var item sessionregistry.Session
			if err := json.Unmarshal(raw, &item); err != nil {
				return err
			}
			if key != nativeRecordKey("session-catalog", item.ID) {
				return fmt.Errorf("not a per-session record: %s", key)
			}
			count++
			return nil
		})
		if err == nil && count != 2 {
			return fmt.Errorf("catalog record count = %d", count)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := events.Append(ctx, session.ID(item.ID), 0, userMessage("retained history")); err != nil {
		t.Fatal(err)
	}
	registry = openCatalog(t, cfg)
	if err := registry.Remove(ctx, item.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Get(ctx, item.ID); !errors.Is(err, sessionregistry.ErrNotFound) {
		t.Fatalf("removed catalog item: %v", err)
	}
	if _, err := os.Stat(item.DataDirectory); err != nil {
		t.Fatalf("remove deleted resources: %v", err)
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	histories, err := app.DiscoverChats(ctx, cfg, session.ID(item.ID))
	if err != nil || len(histories) != 1 || histories[0].Prompt != "retained history" {
		t.Fatalf("remove deleted native history: %+v, %v", histories, err)
	}
	for _, name := range []string{"control.db", "sessions.db"} {
		data, err := os.ReadFile(filepath.Join(cfg.HomeDirectory, name))
		if err != nil || string(data) != "legacy data: preserve" {
			t.Fatalf("legacy file changed: %s, %q, %v", name, data, err)
		}
	}
}

func nativeRecordKey(kind, id string) string {
	return "gsea/v1/" + kind + "/" + base64.RawURLEncoding.EncodeToString([]byte(id))
}

func userMessage(content string) session.PendingEvent {
	return session.PendingEvent{Type: session.EventUserMessage, Payload: session.MustJSONPayload(map[string]string{"content": content})}
}

func TestNativeConversationDiscovery(t *testing.T) {
	ctx := context.Background()
	cfg := catalogConfig(t)
	records, events := nativeCatalogStores(t, cfg)
	prompt := strings.Repeat("界", 250)
	pending := make([]session.PendingEvent, 257)
	for i := range pending {
		pending[i] = session.PendingEvent{Type: session.EventSessionCreated, Payload: json.RawMessage(`{}`)}
	}
	// Find a first prompt beyond one native event page.
	pending = append(pending, userMessage(prompt))
	for start := 0; start < len(pending); start += 64 {
		if _, err := events.Append(ctx, "stable-root", uint64(start), pending[start:min(start+64, len(pending))]...); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []session.ID{"unlocated", "ambiguous", "child-with-custom-id", "agent-thread_hidden"} {
		if _, err := events.Append(ctx, id, 0, userMessage(string(id))); err != nil {
			t.Fatal(err)
		}
	}
	for id, kind := range map[session.ID]session.EventType{"turn-only": session.EventTurnStarted, "doctor-only": session.EventSessionCreated} {
		if _, err := events.Append(ctx, id, 0, session.PendingEvent{Type: kind, Payload: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := records.Update(ctx, func(tx *ateauthority.Tx) error {
		for _, root := range []orchestration.RootThread{
			{SessionID: "stable-root", TeamID: "team_root"},
			{SessionID: "ambiguous", TeamID: "team_ambiguous"},
		} {
			if err := tx.Create(nativeRecordKey("root", string(root.SessionID)), root); err != nil {
				return err
			}
		}
		child := orchestration.AgentSnapshot{ID: "agent_child", ParentAgentID: "agent_parent", SessionID: "child-with-custom-id"}
		if err := tx.Create(nativeRecordKey("agent", string(child.ID)), child); err != nil {
			return err
		}
		for _, view := range []workspacecoord.View{
			{ID: "view_root", TeamID: "team_root", WorkspaceRoot: cfg.WorkspaceDirectory, RootPath: filepath.Join(cfg.HomeDirectory, "snapshot")},
			{ID: "view_ambiguous_1", TeamID: "team_ambiguous", WorkspaceRoot: cfg.WorkspaceDirectory},
			{ID: "view_ambiguous_2", TeamID: "team_ambiguous", WorkspaceRoot: cfg.HomeDirectory},
		} {
			if err := tx.Create(nativeRecordKey("workspace-view", string(view.ID)), view); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	histories, err := app.DiscoverChats(ctx, cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[session.ID]session.ChatHistory)
	for _, history := range histories {
		byID[history.ID] = history
		if history.CreatedAt.IsZero() || history.UpdatedAt.Before(history.CreatedAt) {
			t.Fatalf("invalid history times: %+v", history)
		}
	}
	if len(byID) != 4 {
		t.Fatalf("discovered non-conversations or lost roots: %+v", histories)
	}
	root := byID["stable-root"]
	if root.Prompt != strings.Repeat("界", 240) || root.Workspace != cfg.WorkspaceDirectory {
		t.Fatalf("root prompt/workspace: %+v", root)
	}
	for _, id := range []session.ID{"unlocated", "ambiguous", "turn-only"} {
		if history, ok := byID[id]; !ok || history.Workspace != "" {
			t.Fatalf("unlocated history %q: %+v, %v", id, history, ok)
		}
	}
	exact, err := app.DiscoverChats(ctx, cfg, "stable-root")
	if err != nil || len(exact) != 1 || !reflect.DeepEqual(root, exact[0]) {
		t.Fatalf("exact discovery/reopen: %+v, %v", exact, err)
	}
	for _, id := range []session.ID{"missing", "child-with-custom-id", "doctor-only"} {
		exact, err := app.DiscoverChats(ctx, cfg, id)
		if err != nil || len(exact) != 0 {
			t.Fatalf("unexpected conversation %q: %+v, %v", id, exact, err)
		}
	}
}

func TestNativeCLICatalogAndDiscovery(t *testing.T) {
	ctx := context.Background()
	cfg := catalogConfig(t)
	t.Chdir(cfg.WorkspaceDirectory)
	t.Setenv(config.HomeEnvironment, cfg.HomeDirectory)
	t.Setenv("GSEA_ATE_ENABLED", "true")
	t.Setenv("GSEA_ATE_BINARY", cfg.ATE.Binary)
	t.Setenv("GSEA_ATE_DIRECTORY", cfg.ATE.Directory)
	run := func(arguments ...string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := cli.Run(ctx, arguments, strings.NewReader(""), &stdout, &stderr)
		if code != 0 {
			t.Fatalf("%v exited %d: %s", arguments, code, stderr.String())
		}
		return stdout.String()
	}
	run("session", "create", "--id", "cli-stable", "--name", "Pinned title", "--workspace", cfg.WorkspaceDirectory)
	var shown sessionregistry.Session
	if err := json.Unmarshal([]byte(run("session", "show", "cli-stable")), &shown); err != nil {
		t.Fatal(err)
	}
	if shown.ID != "cli-stable" || shown.Name != "Pinned title" || shown.Workspace != cfg.WorkspaceDirectory {
		t.Fatalf("CLI show: %+v", shown)
	}
	registry := openCatalog(t, cfg)
	other := nativeCatalogItem(cfg, "other-workspace")
	other.Workspace = t.TempDir()
	if err := registry.Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := registry.Create(ctx, nativeCatalogItem(cfg, "empty-session")); err != nil {
		t.Fatal(err)
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	_, events := nativeCatalogStores(t, cfg)
	for _, id := range []session.ID{"cli-stable", "other-workspace", "orphan"} {
		if _, err := events.Append(ctx, id, 0, userMessage("Prompt "+string(id))); err != nil {
			t.Fatal(err)
		}
	}
	var listed []sessionregistry.Session
	if err := json.Unmarshal([]byte(run("session", "list")), &listed); err != nil || len(listed) != 3 {
		t.Fatalf("CLI catalog list: %+v, %v", listed, err)
	}
	local := run("resume", "--list")
	if !strings.Contains(local, "cli-stable") || !strings.Contains(local, "Pinned title") ||
		strings.Contains(local, "other-workspace") || strings.Contains(local, "orphan") || strings.Contains(local, "empty-session") {
		t.Fatalf("workspace discovery: %s", local)
	}
	all := run("resume", "--list", "--all")
	if !strings.Contains(all, "cli-stable") || !strings.Contains(all, "other-workspace") ||
		!strings.Contains(all, "orphan") || strings.Contains(all, "empty-session") {
		t.Fatalf("all workspace discovery: %s", all)
	}
	filtered := run("resume", "--list", "--all", "--data-dir", shown.DataDirectory)
	if !strings.Contains(filtered, "cli-stable") || strings.Contains(filtered, "other-workspace") {
		t.Fatalf("resource directory filter: %s", filtered)
	}
	run("session", "remove", "cli-stable")
	registry = openCatalog(t, cfg)
	if _, err := registry.Get(ctx, "cli-stable"); !errors.Is(err, sessionregistry.ErrNotFound) {
		t.Fatalf("removed CLI session still registered: %v", err)
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(shown.DataDirectory); err != nil {
		t.Fatal(err)
	}
	if after := run("resume", "--list", "--all"); !strings.Contains(after, "cli-stable") {
		t.Fatalf("remove destroyed native history: %s", after)
	}
}

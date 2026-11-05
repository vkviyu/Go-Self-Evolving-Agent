package plugin_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/app"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/plugin"
	pluginwasm "github.com/vkviyu/go-self-evolving-agent/internal/plugin/wasm"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/sqlitestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func TestDurablePluginLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("Go-to-WASM integration fixture")
	}
	source := durableFixture(t)
	ctx := context.Background()
	t.Run("application restart retirement and rollback", func(t *testing.T) {
		cfg := config.Config{DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir(), WASM: pluginwasm.Config{DefaultTimeout: 3 * time.Second}}
		open := func() *app.App {
			application, err := app.Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = application.Close() })
			return application
		}
		application := open()
		manager := application.Plugins()
		v1 := stageDurable(t, manager, "v1", source)
		old, err := manager.Activate(v1)
		if err != nil {
			t.Fatal(err)
		}
		v2 := stageDurable(t, manager, "v2", source)
		active, err := manager.Activate(v2)
		if err != nil {
			t.Fatal(err)
		}
		stageDurable(t, manager, "v3", source)
		invokeDurable(t, old, "v1")
		wantID, err := active.ID()
		if err != nil {
			t.Fatal(err)
		}
		oldID, err := old.ID()
		if err != nil {
			t.Fatal(err)
		}
		if err := application.Close(); err != nil {
			t.Fatal(err)
		}
		application = open()
		manager = application.Plugins()
		active = application.Registry().Snapshot()
		gotID, err := active.ID()
		if err != nil || gotID != wantID {
			t.Fatalf("snapshot identity changed: %s != %s, %v", gotID, wantID, err)
		}
		invokeDurable(t, active, "v2")
		if _, err := application.Registry().SnapshotByID(oldID); !errors.Is(err, tool.ErrSnapshotUnavailable) {
			t.Fatalf("unpublished historical combination was not rejected: %v", err)
		}
		inventory := manager.Inventory()
		if len(inventory) != 3 || inventory[0].State != plugin.StateRetired || inventory[1].State != plugin.StateActive || inventory[2].State != plugin.StateStaged {
			t.Fatalf("inventory: %+v", inventory)
		}
		inventory[0].Manifest.Tool.InputSchema[0] = '!'
		v1, ok := manager.Artifact("echo", "v1")
		if !ok || !json.Valid(v1.Manifest().Tool.InputSchema) || v1.Manifest().Timeout != 3*time.Second {
			t.Fatal("manifest was mutated or default timeout not durable")
		}
		if _, err := manager.Retire("echo", "v1"); err == nil {
			t.Fatal("retired non-active revision")
		}
		if _, err := manager.Retire("echo", "v2"); err != nil {
			t.Fatal(err)
		}
		invokeDurable(t, active, "v2") // Retirement must not unload pinned code.
		if _, ok := application.Registry().Snapshot().Get("echo"); ok {
			t.Fatal("retired revision still published")
		}
		if err := application.Close(); err != nil {
			t.Fatal(err)
		}
		application = open()
		if _, ok := application.Registry().Snapshot().Get("echo"); ok {
			t.Fatal("retirement lost on restart")
		}
		if _, err := application.Plugins().Rollback("echo", "v1"); err != nil {
			t.Fatal(err)
		}
		invokeDurable(t, application.Registry().Snapshot(), "v1")
		if err := application.Close(); err != nil {
			t.Fatal(err)
		}
		application = open()
		invokeDurable(t, application.Registry().Snapshot(), "v1")
		if err := application.Close(); err != nil {
			t.Fatal(err)
		}
		changed := cfg
		changed.WASM.DefaultTimeout = time.Second
		if failed, err := app.Open(ctx, changed); err == nil {
			_ = failed.Close()
			t.Fatal("application accepted changed persisted sandbox limits")
		}
		application = open() // Failed restoration must release database/runtime ownership.
		invokeDurable(t, application.Registry().Snapshot(), "v1")
	})

	for _, committed := range []bool{false, true} {
		name := "publication fails before commit"
		if committed {
			name = "publication result is ambiguous after commit"
		}
		t.Run(name, func(t *testing.T) {
			database := durableDatabase(t)
			catalog := &failingCatalog{Catalog: database.Plugins(), commitThenFail: committed}
			registry := tool.NewRegistry()
			manager := durableManager(t, catalog, registry, pluginwasm.Config{})
			v1 := stageDurable(t, manager, "v1", source)
			old, err := manager.Activate(v1)
			if err != nil {
				t.Fatal(err)
			}
			v2 := stageDurable(t, manager, "v2", source)
			catalog.fail = true
			if _, err := manager.Activate(v2); !errors.Is(err, plugin.ErrPersistenceUncertain) {
				t.Fatalf("activation: %v", err)
			}
			if registry.Snapshot() != old || v1.State() != plugin.StateActive || v2.State() != plugin.StateStaged {
				t.Fatal("failed publication changed live state")
			}
			if _, err := manager.Retire("echo", "v1"); !errors.Is(err, plugin.ErrPersistenceUncertain) {
				t.Fatalf("mutation allowed after uncertain commit: %v", err)
			}
			if _, err := manager.Stage(ctx, durableManifest("v3"), source); !errors.Is(err, plugin.ErrPersistenceUncertain) {
				t.Fatalf("staging allowed after uncertain commit: %v", err)
			}
			invokeDurable(t, old, "v1")
			if err := manager.Close(ctx); err != nil {
				t.Fatal(err)
			}
			restored := tool.NewRegistry()
			durableManager(t, database.Plugins(), restored, pluginwasm.Config{})
			want := "v1"
			if committed {
				want = "v2"
			}
			invokeDurable(t, restored.Snapshot(), want)
		})
	}

	t.Run("restore limits and ownership fail without partial publication", func(t *testing.T) {
		database := durableDatabase(t)
		registry := tool.NewRegistry()
		manager := durableManager(t, database.Plugins(), registry, pluginwasm.Config{})
		if _, err := manager.Activate(stageDurable(t, manager, "v1", source)); err != nil {
			t.Fatal(err)
		}
		if err := manager.Close(ctx); err != nil {
			t.Fatal(err)
		}
		for _, collision := range []bool{false, true} {
			cfg := pluginwasm.Config{DefaultTimeout: time.Second}
			registry := tool.NewRegistry()
			if collision {
				cfg = pluginwasm.Config{}
				if _, err := registry.Activate(nativeEcho()); err != nil {
					t.Fatal(err)
				}
			}
			old := registry.Snapshot()
			wasmRuntime, err := pluginwasm.NewRuntime(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			_, err = plugin.OpenManager(ctx, wasmRuntime, registry, database.Plugins())
			if err == nil {
				t.Fatal("restored incompatible catalog")
			}
			if registry.Snapshot() != old {
				t.Fatal("failed restoration partially published")
			}
			if err := wasmRuntime.Close(ctx); err != nil {
				t.Fatal(err)
			}
		}
	})

	t.Run("reject native takeover cross-owner and reserved names", func(t *testing.T) {
		database := durableDatabase(t)
		registry := tool.NewRegistry()
		manager := durableManager(t, database.Plugins(), registry, pluginwasm.Config{})
		v1 := stageDurable(t, manager, "v1", source)
		foreign := durableManifest("v2")
		foreign.PluginID = "another-plugin"
		if _, err := manager.Stage(ctx, foreign, source); !errors.Is(err, plugin.ErrToolOwned) {
			t.Fatalf("cross-owner stage: %v", err)
		}
		reserved := durableManifest("v1")
		reserved.Tool.Name = "agent_spawn"
		if _, err := manager.Stage(ctx, reserved, source); !errors.Is(err, plugin.ErrToolOwned) {
			t.Fatalf("reserved name: %v", err)
		}
		if _, err := registry.Activate(nativeEcho()); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.Activate(v1); !errors.Is(err, plugin.ErrToolOwned) {
			t.Fatalf("native installed between stage and activate: %v", err)
		}
		if _, err := manager.Stage(ctx, durableManifest("v2"), source); !errors.Is(err, plugin.ErrToolOwned) {
			t.Fatalf("native takeover at stage: %v", err)
		}
		items, err := database.Plugins().Load(ctx)
		if err != nil || len(items) != 1 || items[0].State != plugin.StateStaged {
			t.Fatalf("ownership failure mutated durable state: %+v, %v", items, err)
		}
	})

	t.Run("late invalid module prevents all restoration publication", func(t *testing.T) {
		database := durableDatabase(t)
		manager := durableManager(t, database.Plugins(), tool.NewRegistry(), pluginwasm.Config{})
		if _, err := manager.Activate(stageDurable(t, manager, "v1", source)); err != nil {
			t.Fatal(err)
		}
		if err := manager.Close(ctx); err != nil {
			t.Fatal(err)
		}
		items, err := database.Plugins().Load(ctx)
		if err != nil {
			t.Fatal(err)
		}
		bad := items[0].Record
		bad.Manifest.Tool.Name = "z_invalid"
		invalidWASM := []byte("invalid WASM with a valid content checksum")
		sum := sha256.Sum256(invalidWASM)
		bad.ContentSHA256 = hex.EncodeToString(sum[:])
		if err := database.Plugins().Save(ctx, bad, invalidWASM); err != nil {
			t.Fatal(err)
		}
		if err := database.Plugins().SetActive(ctx, "z_invalid", "", "v1"); err != nil {
			t.Fatal(err)
		}
		registry := tool.NewRegistry()
		runtime, err := pluginwasm.NewRuntime(ctx, pluginwasm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		defer runtime.Close(ctx)
		if _, err := plugin.OpenManager(ctx, runtime, registry, database.Plugins()); err == nil {
			t.Fatal("restored invalid module")
		}
		if registry.Snapshot().Generation() != 0 {
			t.Fatal("valid prefix of catalog was published")
		}
	})

	t.Run("ambiguous staging is retained but never activated", func(t *testing.T) {
		database := durableDatabase(t)
		catalog := &failingSaveCatalog{Catalog: database.Plugins()}
		registry := tool.NewRegistry()
		manager := durableManager(t, catalog, registry, pluginwasm.Config{})
		if _, err := manager.Stage(ctx, durableManifest("v1"), source); !errors.Is(err, plugin.ErrPersistenceUncertain) {
			t.Fatalf("stage: %v", err)
		}
		if len(manager.Inventory()) != 0 || registry.Snapshot().Generation() != 0 {
			t.Fatal("failed stage published artifact")
		}
		if err := manager.Close(ctx); err != nil {
			t.Fatal(err)
		}
		restored := tool.NewRegistry()
		manager = durableManager(t, database.Plugins(), restored, pluginwasm.Config{})
		if entries := manager.Inventory(); len(entries) != 1 || entries[0].State != plugin.StateStaged {
			t.Fatalf("committed stage not restored: %+v", entries)
		}
		if _, exists := restored.Snapshot().Get("echo"); exists {
			t.Fatal("staging implicitly activated")
		}
	})

	t.Run("concurrent staging retains one immutable revision", func(t *testing.T) {
		database := durableDatabase(t)
		manager := durableManager(t, database.Plugins(), tool.NewRegistry(), pluginwasm.Config{})
		var group sync.WaitGroup
		results := make(chan error, 2)
		for range 2 {
			group.Go(func() {
				_, err := manager.Stage(ctx, durableManifest("v1"), source)
				results <- err
			})
		}
		group.Wait()
		close(results)
		success, duplicates := 0, 0
		for err := range results {
			if err == nil {
				success++
			} else if errors.Is(err, plugin.ErrArtifactExists) {
				duplicates++
			} else {
				t.Fatal(err)
			}
		}
		if success != 1 || duplicates != 1 || len(manager.Inventory()) != 1 {
			t.Fatalf("success=%d duplicates=%d", success, duplicates)
		}
		snapshot, err := manager.Rollback("echo", "v1")
		if err != nil {
			t.Fatal(err)
		}
		invokeDurable(t, snapshot, "v1")
	})
}

type failingSaveCatalog struct{ plugin.Catalog }

func (c *failingSaveCatalog) Save(ctx context.Context, record plugin.RevisionRecord, source []byte) error {
	if err := c.Catalog.Save(ctx, record, source); err != nil {
		return err
	}
	return errors.New("injected lost stage receipt")
}

type failingCatalog struct {
	plugin.Catalog
	fail, commitThenFail bool
}

func (c *failingCatalog) SetActive(ctx context.Context, name, expected, next string) error {
	if !c.fail {
		return c.Catalog.SetActive(ctx, name, expected, next)
	}
	if c.commitThenFail {
		if err := c.Catalog.SetActive(ctx, name, expected, next); err != nil {
			return err
		}
	}
	return errors.New("injected storage failure")
}

func durableDatabase(t *testing.T) *sqlitestore.Database {
	t.Helper()
	database, err := sqlitestore.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func durableManager(t *testing.T, catalog plugin.Catalog, registry *tool.Registry, cfg pluginwasm.Config) *plugin.Manager {
	t.Helper()
	runtime, err := pluginwasm.NewRuntime(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	manager, err := plugin.OpenManager(context.Background(), runtime, registry, catalog, "agent_")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	return manager
}

func durableManifest(revision string) plugin.Manifest {
	return plugin.Manifest{PluginID: "echo-plugin", Tool: tool.Descriptor{Name: "echo", Revision: revision, Description: "Echo JSON", InputSchema: []byte(`{"type":"object"}`), RiskClass: tool.RiskPure}}
}

func nativeEcho() tool.Binding {
	return tool.Binding{Descriptor: durableManifest("native").Tool, Executor: tool.InvokeFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) { return input, nil })}
}

func stageDurable(t *testing.T, manager *plugin.Manager, revision string, source []byte) *plugin.Artifact {
	t.Helper()
	artifact, err := manager.Stage(context.Background(), durableManifest(revision), source)
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}

func invokeDurable(t *testing.T, snapshot *tool.Snapshot, revision string) {
	t.Helper()
	binding, ok := snapshot.Get("echo")
	if !ok || binding.Descriptor.Revision != revision {
		t.Fatalf("revision = %s, want %s", binding.Descriptor.Revision, revision)
	}
	input := json.RawMessage(`{"value":"persistent"}`)
	output, err := binding.Executor.Invoke(context.Background(), input)
	if err != nil || string(output) != string(input) {
		t.Fatalf("invoke: %s, %v", output, err)
	}
}

func durableFixture(t *testing.T) []byte {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate fixture")
	}
	path := filepath.Join(t.TempDir(), "echo.wasm")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "build", "-buildmode=c-shared", "-o", path, "./wasm/testdata/echo")
	command.Dir = filepath.Dir(filename)
	command.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0", "GOWORK=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v\n%s", err, output)
	}
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return source
}

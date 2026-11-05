package plugin

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	pluginwasm "github.com/vkviyu/go-self-evolving-agent/internal/plugin/wasm"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func TestStageActivateHotSwapAndRollback(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Go-to-WASM plugin manager integration build in short mode")
	}
	source := buildFixture(t)
	ctx := context.Background()
	wasmRuntime, err := pluginwasm.NewRuntime(ctx, pluginwasm.Config{DefaultTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry()
	manager, err := NewManager(wasmRuntime, registry)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(context.Background()); err != nil {
			t.Error(err)
		}
		if err := wasmRuntime.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})

	first, err := manager.Stage(ctx, manifest("rev-1"), source)
	if err != nil {
		t.Fatal(err)
	}
	if first.State() != StateStaged || first.ContentSHA256() == "" {
		t.Fatalf("unexpected staged artifact: state=%q sha=%q", first.State(), first.ContentSHA256())
	}
	if _, err := manager.Activate(first); err != nil {
		t.Fatal(err)
	}
	oldSnapshot := registry.Snapshot()

	second, err := manager.Stage(ctx, manifest("rev-2"), source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Activate(second); err != nil {
		t.Fatal(err)
	}
	newSnapshot := registry.Snapshot()
	if first.State() != StateRetired || second.State() != StateActive {
		t.Fatalf("unexpected states: first=%q second=%q", first.State(), second.State())
	}
	assertRevisionInvokes(t, oldSnapshot, "rev-1")
	assertRevisionInvokes(t, newSnapshot, "rev-2")

	if _, err := manager.Rollback("echo", "rev-1"); err != nil {
		t.Fatal(err)
	}
	assertRevisionInvokes(t, registry.Snapshot(), "rev-1")
}

func manifest(revision string) Manifest {
	return Manifest{
		PluginID: "echo-plugin",
		Tool: tool.Descriptor{
			Name:        "echo",
			Description: "Echoes JSON through a staged WASM revision.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":true}`),
			Revision:    revision,
			RiskClass:   tool.RiskPure,
		},
	}
}

func assertRevisionInvokes(t *testing.T, snapshot *tool.Snapshot, revision string) {
	t.Helper()
	binding, ok := snapshot.Get("echo")
	if !ok || binding.Descriptor.Revision != revision {
		t.Fatalf("snapshot revision = %q, want %q", binding.Descriptor.Revision, revision)
	}
	request := json.RawMessage(`{"value":"hot"}`)
	result, err := binding.Executor.Invoke(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != string(request) {
		t.Fatalf("result = %s, want %s", result, request)
	}
}

func buildFixture(t *testing.T) []byte {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate plugin manager package")
	}
	packageDirectory := filepath.Dir(filename)
	wasmPath := filepath.Join(t.TempDir(), "echo.wasm")
	command := exec.Command("go", "build", "-buildmode=c-shared", "-o", wasmPath, "./wasm/testdata/echo")
	command.Dir = packageDirectory
	command.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0", "GOWORK=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build Go WASM fixture: %v\n%s", err, output)
	}
	source, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatal(err)
	}
	return source
}

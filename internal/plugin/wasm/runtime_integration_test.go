package wasm

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestGo127PluginRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Go-to-WASM integration build in short mode")
	}
	if !strings.HasPrefix(runtime.Version(), "go1.27") {
		t.Fatalf("test requires Go 1.27, got %s", runtime.Version())
	}

	wasmPath := filepath.Join(t.TempDir(), "echo.wasm")
	command := exec.Command("go", "build", "-buildmode=c-shared", "-o", wasmPath, "./testdata/echo")
	command.Dir = packageDirectory(t)
	command.Env = append(os.Environ(),
		"GOOS=wasip1",
		"GOARCH=wasm",
		"CGO_ENABLED=0",
		"GOWORK=off",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build Go WASM fixture: %v\n%s", err, output)
	}
	source, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	runtime, err := NewRuntime(ctx, Config{DefaultTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	module, err := runtime.Compile(ctx, Revision{PluginID: "echo", Revision: "test-1"}, source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = module.Close(context.Background()) })

	request := json.RawMessage(`{"message":"hello from Go 1.27"}`)
	result, err := module.Invoke(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != string(request) {
		t.Fatalf("result = %s, want %s", result, request)
	}

	timeoutModule, err := runtime.Compile(ctx, Revision{
		PluginID: "echo", Revision: "timeout-test", Timeout: 25 * time.Millisecond,
	}, source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = timeoutModule.Close(context.Background()) })
	started := time.Now()
	if _, err := timeoutModule.Invoke(ctx, json.RawMessage(`{"loop":true}`)); err == nil {
		t.Fatal("infinite WASM call unexpectedly succeeded")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("WASM timeout took %s", elapsed)
	}
}

func packageDirectory(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate WASM package directory")
	}
	return filepath.Dir(filename)
}

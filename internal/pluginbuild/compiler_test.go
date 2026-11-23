package pluginbuild

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/plugin/wasm"
)

const echoSource = "package main\nimport \"encoding/json\"\nfunc Invoke(input json.RawMessage)(json.RawMessage,error){return input,nil}\n"

func TestCandidateSourcePolicy(t *testing.T) {
	if err := ValidateSource(echoSource); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{
		strings.Replace(echoSource, "encoding/json", "os/exec", 1),
		echoSource + "\n//go:linkname x y\n", echoSource + "\nfunc init(){}", echoSource + "\nfunc main(){}",
		strings.Replace(echoSource, "\"encoding/json\"", "x \"encoding/json\"", 1), strings.Repeat("x", (64<<10)+1),
	} {
		if err := ValidateSource(source); err == nil {
			t.Fatalf("accepted unsafe source: %.100s", source)
		}
	}
}

func TestCompilerRequiresSelectedToolchain(t *testing.T) {
	for _, root := range []string{"", "relative/toolchain", filepath.Join(t.TempDir(), "missing"), t.TempDir()} {
		artifact, evidence, err := (Compiler{GoRoot: root}).Compile(t.Context(), echoSource)
		if artifact != nil || evidence.Success || !errors.Is(err, ErrToolchainUnavailable) {
			t.Fatalf("Compile with root %q produced success=%v, err=%v", root, evidence.Success, err)
		}
		if !strings.Contains(err.Error(), "Set GSEA_GO_ROOT to the absolute path of a Go 1.27.0 installation") {
			t.Fatalf("Compile with root %q did not explain how to select a toolchain: %v", root, err)
		}
	}
}

func TestIsolatedCompilerProducesInvokableWASM(t *testing.T) {
	if testing.Short() {
		t.Skip("real isolated Go compiler")
	}
	goRoot := runtime.GOROOT() // Explicit fixture selected by the test harness.
	path := t.TempDir()
	for _, name := range []string{"sandbox-exec", "bwrap", "bubblewrap"} {
		if helper, err := exec.LookPath(name); err == nil {
			if err := os.Symlink(helper, filepath.Join(path, name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Setenv("PATH", path)
	t.Setenv("GOROOT", filepath.Join(t.TempDir(), "external-go"))
	t.Setenv("GOTOOLCHAIN", "go1.99.0")
	ctx := t.Context()
	source, evidence, err := (Compiler{GoRoot: goRoot}).Compile(ctx, echoSource)
	if err != nil {
		t.Fatalf("compile: %v\n%s", err, evidence.Log)
	}
	if !evidence.Success || !strings.Contains(evidence.Toolchain, "go"+GoVersion+" ") || evidence.SDKSHA256 == "" || evidence.ToolchainSHA256 == "" || evidence.Sandbox == "" || evidence.ToolchainHashScope != toolchainHashScope || evidence.MemoryLimitMode == "" {
		t.Fatalf("missing evidence: %+v", evidence)
	}
	wasmRuntime, err := wasm.NewRuntime(ctx, wasm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer wasmRuntime.Close(context.Background())
	module, err := wasmRuntime.Compile(ctx, wasm.Revision{PluginID: "test", Revision: "v1"}, source)
	if err != nil {
		t.Fatal(err)
	}
	defer module.Close(ctx)
	result, err := module.Invoke(ctx, []byte(`{"echo":"ok"}`))
	if err != nil || string(result) != `{"echo":"ok"}` {
		t.Fatalf("result=%s err=%v", result, err)
	}
}

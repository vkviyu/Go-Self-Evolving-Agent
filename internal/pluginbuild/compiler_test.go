package pluginbuild

import (
	"context"
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

func TestIsolatedCompilerProducesInvokableWASM(t *testing.T) {
	if testing.Short() {
		t.Skip("real isolated Go compiler")
	}
	ctx := context.Background()
	source, evidence, err := (Compiler{}).Compile(ctx, echoSource)
	if err != nil {
		t.Fatalf("compile: %v\n%s", err, evidence.Log)
	}
	if !evidence.Success || evidence.SDKSHA256 == "" || evidence.ToolchainSHA256 == "" || evidence.Sandbox == "" || evidence.ToolchainHashScope != toolchainHashScope || evidence.MemoryLimitMode == "" {
		t.Fatalf("missing evidence: %+v", evidence)
	}
	runtime, err := wasm.NewRuntime(ctx, wasm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(ctx)
	module, err := runtime.Compile(ctx, wasm.Revision{PluginID: "test", Revision: "v1"}, source)
	if err != nil {
		t.Fatal(err)
	}
	defer module.Close(ctx)
	result, err := module.Invoke(ctx, []byte(`{"echo":"ok"}`))
	if err != nil || string(result) != `{"echo":"ok"}` {
		t.Fatalf("result=%s err=%v", result, err)
	}
}

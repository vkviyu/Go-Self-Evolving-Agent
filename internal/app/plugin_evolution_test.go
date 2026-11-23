package app

import (
	"context"
	"encoding/json"
	"runtime"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/approval"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/plugin"
	"github.com/vkviyu/go-self-evolving-agent/internal/pluginbuild"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func TestGeneratedPluginBuildVerifyApproveAndReopen(t *testing.T) {
	if testing.Short() {
		t.Skip("isolated Go to WASM evolution integration")
	}
	t.Setenv(pluginbuild.EnvGoRoot, runtime.GOROOT())
	ctx := context.Background()
	cfg := testConfig(t, config.Config{DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir()})
	application, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	request := pluginbuild.Request{OwnerSession: "owner", Manifest: plugin.Manifest{PluginID: "test-extension", Tool: tool.Descriptor{Name: "extension_echo", Revision: "v1", Description: "Echo JSON", RiskClass: tool.RiskPure, InputSchema: []byte(`{"type":"object"}`)}}, Source: "package main\nimport \"encoding/json\"\nfunc Invoke(input json.RawMessage)(json.RawMessage,error){return input,nil}\n", Cases: []plugin.TestCase{{Name: "echo", Input: []byte(`{"ok":true}`), Expected: []byte(`{"ok":true}`)}}}
	old := application.Registry().Snapshot()
	result, err := application.BuildPlugin(ctx, request)
	if err != nil || result.State != "awaiting_approval" || result.TestReportID == "" {
		t.Fatalf("build=%+v err=%v", result, err)
	}
	if _, exists := application.Registry().Snapshot().Get("extension_echo"); exists {
		t.Fatal("candidate published itself")
	}
	stored, _, err := application.PluginBuild(ctx, result.BuildID)
	if err != nil || stored.Manifest.Timeout != 5*time.Second {
		t.Fatalf("omitted timeout inherited unsafe global default: %v %v", stored.Manifest.Timeout, err)
	}
	repeated, err := application.BuildPlugin(ctx, request)
	if err != nil || repeated.BuildID != result.BuildID {
		t.Fatalf("build replay=%+v %v", repeated, err)
	}
	args, _ := json.Marshal(map[string]any{"build_id": result.BuildID})
	call := tool.Call{SessionID: "owner", TurnID: "turn", CallID: "publish-denied", ToolName: "plugin_publish", Registry: old, Arguments: args}
	if _, err := application.Pipeline().WithApprover(approval.DenyApprover{}).Execute(ctx, call); err == nil {
		t.Fatal("published without approval")
	}
	call.SessionID, call.CallID = "foreign", "inspect-foreign"
	call.ToolName = "plugin_inspect"
	if _, err := application.Pipeline().Execute(ctx, call); err == nil {
		t.Fatal("foreign session read build")
	}
	call.SessionID, call.CallID, call.ToolName = "owner", "publish-approved", "plugin_publish"
	if _, err := application.Pipeline().WithApprover(approval.NewToolAllowlistApprover("plugin_publish")).Execute(ctx, call); err != nil {
		t.Fatal(err)
	}
	if _, exists := old.Get("extension_echo"); exists {
		t.Fatal("in-flight snapshot changed")
	}
	current := application.Registry().Snapshot()
	if _, exists := current.Get("extension_echo"); !exists {
		t.Fatal("new tool not published")
	}
	call = tool.Call{SessionID: "owner", TurnID: "next", CallID: "invoke", ToolName: "extension_echo", Registry: current, Arguments: []byte(`{"hello":"world"}`)}
	if _, err := application.Pipeline().WithApprover(approval.DenyApprover{}).Execute(ctx, call); err == nil {
		t.Fatal("extension invocation bypassed policy")
	}
	call.CallID = "invoke-approved"
	output, err := application.Pipeline().WithApprover(approval.NewToolAllowlistApprover("extension_echo")).Execute(ctx, call)
	if err != nil || string(output.Data) != `{"hello":"world"}` {
		t.Fatalf("output=%s err=%v", output.Data, err)
	}
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, exists := reopened.Registry().Snapshot().Get("extension_echo"); !exists {
		t.Fatal("publication did not survive restart")
	}
	if _, err := reopened.PromotePlugin(ctx, "extension_echo", "v1", result.TestReportID, false); err == nil {
		t.Fatal("promotion did not require explicit confirmation")
	}
	if _, _, err := reopened.PluginBuild(ctx, result.BuildID); err != nil {
		t.Fatal(err)
	}
	if _, err := PreviewRetention(ctx, cfg, "owner"); err == nil {
		t.Fatal("maintenance bypassed owner lock")
	}
	if _, err := CollectPlugin(ctx, cfg, "extension_echo", "v1", "fake", true); err == nil {
		t.Fatal("collection bypassed owner lock")
	}
}

func TestPluginProposalArgumentsCannotGrantCapabilities(t *testing.T) {
	request := pluginbuild.Request{Manifest: plugin.Manifest{PluginID: "x", Tool: tool.Descriptor{Name: "extension_x", Revision: "v1", Description: "x", RiskClass: tool.RiskWrite, InputSchema: []byte(`{"type":"object"}`)}}}
	if err := request.Validate(); err == nil {
		t.Fatal("generated code granted workspace writes")
	}
}

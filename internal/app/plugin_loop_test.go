package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/approval"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/plugin"
	"github.com/vkviyu/go-self-evolving-agent/internal/pluginbuild"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

// A deterministic model drives the real Eino loop, compiler, WASM tests and
// policy pipeline. This proves control flow, not model coding proficiency.
type pluginLoopModel struct {
	step     int
	buildIDs []string
}

func (m *pluginLoopModel) Generate(_ context.Context, messages []*schema.Message, options ...model.Option) (*schema.Message, error) {
	step := m.step
	m.step++
	invoke := func(name string, args any) (*schema.Message, error) {
		raw, err := json.Marshal(args)
		if err != nil {
			return nil, err
		}
		return schema.AssistantMessage("", []schema.ToolCall{{ID: fmt.Sprintf("tool-%d", step), Type: "function", Function: schema.FunctionCall{Name: name, Arguments: string(raw)}}}), nil
	}
	request := pluginbuild.Request{Manifest: plugin.Manifest{PluginID: "loop", Timeout: time.Second, Tool: tool.Descriptor{Name: "extension_loop", Revision: fmt.Sprintf("v%d", step+1), Description: "echo", RiskClass: tool.RiskPure, InputSchema: json.RawMessage(`{"type":"object"}`)}}, Source: "package main\nimport \"encoding/json\"\nfunc Invoke(input json.RawMessage)(json.RawMessage,error){return input,nil}\n", Cases: []plugin.TestCase{{Name: "echo", Input: json.RawMessage(`{"ok":true}`), Expected: json.RawMessage(`{"ok":true}`)}}}
	last := messages[len(messages)-1]
	if step >= 1 && step <= 3 {
		var built pluginbuild.Result
		if err := json.Unmarshal([]byte(last.Content), &built); err != nil {
			return nil, err
		}
		expected := []string{"", "failed", "quarantined", "awaiting_approval"}[step]
		if built.State != expected || built.BuildID == "" {
			return nil, fmt.Errorf("step %d expected %s: %s", step, expected, last.Content)
		}
		m.buildIDs = append(m.buildIDs, built.BuildID)
		if step == 3 {
			return invoke("plugin_publish", map[string]string{"build_id": built.BuildID})
		}
	}
	switch step {
	case 0:
		request.Source = strings.Replace(request.Source, "return input,nil", "return missingSymbol,nil", 1)
		return invoke("plugin_propose", request)
	case 1:
		request.Cases[0].Expected = json.RawMessage(`{"wrong":true}`)
		return invoke("plugin_propose", request)
	case 2:
		return invoke("plugin_propose", request)
	case 4:
		if !strings.Contains(last.Content, `"available_next_turn":true`) {
			return nil, errors.New("publication did not succeed")
		}
		for _, info := range model.GetCommonOptions(nil, options...).Tools {
			if info.Name == "extension_loop" {
				return nil, errors.New("current Turn gained the published tool")
			}
		}
		return schema.AssistantMessage("published for next Turn", nil), nil
	case 5:
		found := false
		for _, info := range model.GetCommonOptions(nil, options...).Tools {
			found = found || info.Name == "extension_loop"
		}
		if !found {
			return nil, errors.New("next Turn did not receive tool")
		}
		return invoke("extension_loop", map[string]bool{"ok": true})
	case 6:
		if last.Content != `{"ok":true}` {
			return nil, fmt.Errorf("plugin observation=%s", last.Content)
		}
		return schema.AssistantMessage("extension verified", nil), nil
	}
	return nil, errors.New("unexpected model replay")
}
func (m *pluginLoopModel) Stream(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := m.Generate(ctx, input, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func TestEinoGeneratedToolRepairQuarantinePublishAndNextTurn(t *testing.T) {
	if testing.Short() {
		t.Skip("three real isolated Go builds")
	}
	ctx := context.Background()
	cfg := config.Config{DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir()}
	a, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	fake := &pluginLoopModel{}
	factory, err := a.NewAgentFactory(fake, agentruntime.Config{}, approval.NewToolAllowlistApprover("plugin_propose", "plugin_publish", "extension_loop"))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"published for next Turn", "extension verified"} {
		turn, err := factory.NewTurn(ctx, "loop")
		if err != nil {
			t.Fatal(err)
		}
		result, err := turn.Run(ctx, []*schema.Message{schema.UserMessage("perform the authorized test workflow")}, nil)
		if err != nil || result.Final == nil || result.Final.Content != expected {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	}
	if fake.step != 7 || len(fake.buildIDs) != 3 {
		t.Fatalf("loop=%+v", fake)
	}
	for _, id := range fake.buildIDs {
		request, result, err := a.PluginBuild(ctx, id)
		if err != nil || request.OwnerSession != "loop" || result == nil {
			t.Fatal("build diagnostics not retained", err)
		}
	}
	_, published, err := a.PluginBuild(ctx, fake.buildIDs[2])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Plugins().Retire("extension_loop", "v3"); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	held, err := PreviewPluginCollection(ctx, cfg, "extension_loop", "v3")
	if err != nil || len(held.Blockers) == 0 {
		t.Fatalf("finished Turn lost its retained plugin: %+v %v", held, err)
	}
	plan, err := PreviewRetention(ctx, cfg, "loop")
	if err != nil || len(plan.Blockers) != 0 || len(plan.Consumers) != 2 {
		t.Fatalf("archive plan=%+v err=%v", plan, err)
	}
	if _, err := ArchiveSession(ctx, cfg, "loop", plan.ID, true); err != nil {
		t.Fatal(err)
	}
	collect, err := PreviewPluginCollection(ctx, cfg, "extension_loop", "v3")
	if err != nil || len(collect.Blockers) != 0 {
		t.Fatalf("collection plan=%+v err=%v", collect, err)
	}
	if _, err := CollectPlugin(ctx, cfg, "extension_loop", "v3", collect.ID, true); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, exists := reopened.Plugins().Artifact("extension_loop", "v3"); exists {
		t.Fatal("collected revision restored")
	}
	if _, exists := reopened.Plugins().Artifact("extension_loop", "v2"); !exists {
		t.Fatal("shared candidate lost during collection")
	}
	if _, err := reopened.PromotePlugin(ctx, "extension_loop", "v3", published.TestReportID, true); err == nil {
		t.Fatal("deleted revision republished from old report")
	}
	if _, _, err := reopened.PluginBuild(ctx, fake.buildIDs[2]); err != nil {
		t.Fatal("collection deleted build evidence", err)
	}
}

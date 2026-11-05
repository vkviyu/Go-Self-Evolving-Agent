package einoadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/approval"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
	appTool "github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func TestToolUsesTurnScopedSnapshot(t *testing.T) {
	t.Parallel()

	registry := appTool.NewRegistry()
	if _, err := registry.Activate(binding("rev-1")); err != nil {
		t.Fatal(err)
	}
	turnSnapshot := registry.Snapshot()
	if _, err := registry.Activate(binding("rev-2")); err != nil {
		t.Fatal(err)
	}
	engine, err := policy.DefaultDeny(policy.Rule{
		ID:                "test-allow",
		ToolPattern:       "echo",
		CapabilityPattern: "tool.invoke",
		Effect:            policy.EffectAllow,
	})
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := appTool.NewPipeline(appTool.PipelineConfig{Registry: registry, Policy: engine, Events: memorystore.New()})
	if err != nil {
		t.Fatal(err)
	}

	tools, err := NewTools(pipeline, turnSnapshot, "session-1", "runtime-1", "turn-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(tools))
	}
	info, err := tools[0].Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "echo" || info.Extra["revision"] != "rev-1" {
		t.Fatalf("unexpected info: %#v", info)
	}

	_, ok := tools[0].(einotool.InvokableTool)
	if !ok {
		t.Fatal("adapter is not invokable")
	}
	result, err := invokeThroughEino(context.Background(), tools[0], `{"message":"hello"}`)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]string
	if err := json.Unmarshal([]byte(result), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["revision"] != "rev-1" {
		t.Fatalf("executed revision %q, want rev-1", decoded["revision"])
	}
}

func TestToolReturnsRecordedExecutionFailureToModel(t *testing.T) {
	t.Parallel()

	registry := appTool.NewRegistry()
	failing := binding("rev-1")
	failing.Executor = appTool.InvokeFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return nil, errors.New("unknown task")
	})
	if _, err := registry.Activate(failing); err != nil {
		t.Fatal(err)
	}
	engine, err := policy.DefaultDeny(policy.Rule{
		ID: "test-allow", ToolPattern: "echo", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow,
	})
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := appTool.NewPipeline(appTool.PipelineConfig{Registry: registry, Policy: engine, Events: memorystore.New()})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := NewTools(pipeline, registry.Snapshot(), "session-1", "runtime-1", "turn-1")
	if err != nil {
		t.Fatal(err)
	}
	result, err := invokeThroughEino(context.Background(), tools[0], `{"message":"hello"}`)
	if err != nil {
		t.Fatalf("recorded execution failure aborted turn: %v", err)
	}
	var decoded map[string]string
	if err := json.Unmarshal([]byte(result), &decoded); err != nil {
		t.Fatalf("failure result is not JSON: %v", err)
	}
	if decoded["error"] == "" || decoded["message"] == "" {
		t.Fatalf("failure result = %#v", decoded)
	}
}

func TestToolReturnsResultWhenApprovalIsDenied(t *testing.T) {
	t.Parallel()

	registry := appTool.NewRegistry()
	if _, err := registry.Activate(binding("rev-1")); err != nil {
		t.Fatal(err)
	}
	engine, err := policy.DefaultDeny(policy.Rule{
		ID:                "test-approval",
		ToolPattern:       "echo",
		CapabilityPattern: "tool.invoke",
		Effect:            policy.EffectRequireApproval,
	})
	if err != nil {
		t.Fatal(err)
	}
	broker := approval.NewBroker(1)
	t.Cleanup(func() { _ = broker.Close() })
	pipeline, err := appTool.NewPipeline(appTool.PipelineConfig{
		Registry: registry,
		Policy:   engine,
		Approver: broker,
		Events:   memorystore.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := NewTools(pipeline, registry.Snapshot(), "session-1", "runtime-1", "turn-1")
	if err != nil {
		t.Fatal(err)
	}
	type response struct {
		value string
		err   error
	}
	responses := make(chan response, 1)
	go func() {
		value, invokeErr := invokeThroughEino(context.Background(), tools[0], `{"message":"hello"}`)
		responses <- response{value: value, err: invokeErr}
	}()
	request := <-broker.Requests()
	if err := broker.Resolve(request.ID, false); err != nil {
		t.Fatal(err)
	}
	result := <-responses
	if result.err != nil {
		t.Fatalf("denied invocation returned error: %v", result.err)
	}
	var decoded map[string]string
	if err := json.Unmarshal([]byte(result.value), &decoded); err != nil {
		t.Fatalf("denied result is not JSON: %v", err)
	}
	if decoded["error"] != appTool.ErrApprovalDenied.Error() {
		t.Fatalf("denied result error = %q", decoded["error"])
	}
}

func invokeThroughEino(ctx context.Context, base einotool.BaseTool, arguments string) (string, error) {
	info, err := base.Info(ctx)
	if err != nil {
		return "", err
	}
	node, err := compose.NewToolNode(ctx, &compose.ToolsNodeConfig{Tools: []einotool.BaseTool{base}, ExecuteSequentially: true})
	if err != nil {
		return "", err
	}
	results, err := node.Invoke(ctx, schema.AssistantMessage("", []schema.ToolCall{{
		ID: "test-call", Type: "function", Function: schema.FunctionCall{Name: info.Name, Arguments: arguments},
	}}))
	if err != nil {
		return "", err
	}
	if len(results) != 1 {
		return "", fmt.Errorf("expected one tool result, got %d", len(results))
	}
	return results[0].Content, nil
}

func binding(revision string) appTool.Binding {
	return appTool.Binding{
		Descriptor: appTool.Descriptor{
			Name:        "echo",
			Description: "Returns its input for adapter tests.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"message":{"type":"string"}}}`),
			Revision:    revision,
			RiskClass:   appTool.RiskPure,
		},
		Executor: appTool.InvokeFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
			return json.Marshal(map[string]string{"revision": revision})
		}),
	}
}

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/atebridge"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/experience"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

type memoryToolApprover struct{ calls int }

func (a *memoryToolApprover) Approve(_ context.Context, request policy.Request, _ policy.Decision) (bool, error) {
	a.calls++
	return request.ToolName == "memory_manage" && request.RiskClass == string(tool.RiskWrite), nil
}

func TestMemoryToolBindingsThroughNativeAppPipeline(t *testing.T) {
	a, err := Open(t.Context(), testConfig(t, config.Config{DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir()}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Error(err)
		}
	})
	root, _, err := a.EnsureRootThread(t.Context(), "memory-binding-session")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.events.Append(t.Context(), root.SessionID, session.AnySequence, session.PendingEvent{
		Type: session.EventUserMessage, CorrelationID: "memory-binding-turn",
		Payload: session.MustJSONPayload(map[string]string{"content": "I prefer concise responses."}),
	}); err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry()
	bindings := memoryToolBindings(a.experience)
	if len(bindings) != 2 || len(memoryToolBindings(nil)) != 0 {
		t.Fatal("unexpected memory tool bindings")
	}
	for _, binding := range bindings {
		if _, err := registry.Activate(binding); err != nil {
			t.Fatal(err)
		}
		if _, err := binding.Executor.Invoke(t.Context(), json.RawMessage(`{"operation":"count"}`)); err == nil {
			t.Fatal("binding accepted direct invocation without trusted envelope")
		}
	}
	engine, err := policy.DefaultDeny(
		policy.Rule{ID: "memory-read", ToolPattern: "memory_query", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow},
		policy.Rule{ID: "memory-write", ToolPattern: "memory_manage", CapabilityPattern: "tool.invoke", Effect: policy.EffectRequireApproval},
	)
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := tool.NewPipeline(tool.PipelineConfig{Registry: registry, Policy: engine, Events: a.events})
	if err != nil {
		t.Fatal(err)
	}
	call := tool.Call{
		Actor: root.ActorScope(), SessionID: root.SessionID, TurnID: "memory-binding-turn",
		CallID: "denied", ToolName: "memory_manage",
		Arguments: json.RawMessage(`{"operation":"create_candidate","kind":"preference","claim":"The user states a preference for concise responses.","new_space":{"name":"Response preferences","description":"Scoped communication preferences."}}`),
	}
	if _, err := pipeline.Execute(t.Context(), call); !errors.Is(err, policy.ErrApprovalUnavailable) {
		t.Fatalf("mutation did not require approval: %v", err)
	}
	approver := &memoryToolApprover{}
	pipeline = pipeline.WithApprover(approver)
	call.CallID = "approved"
	result, err := pipeline.Execute(t.Context(), call)
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Ref     string `json:"ref"`
		Status  string `json:"status"`
		Adopted bool   `json:"adopted"`
	}
	if err := json.Unmarshal(result.Data, &saved); err != nil || saved.Status != "candidate" || saved.Ref == "" || saved.Adopted || approver.calls != 1 {
		t.Fatalf("unexpected mutation result: %s %v", result.Data, err)
	}
	// The executor receives the actor/session/turn/call from Pipeline, never
	// JSON arguments. Exact candidate reads return text and no adoption.
	call.CallID, call.ToolName = "read", "memory_query"
	call.Arguments, _ = json.Marshal(map[string]any{"operation": "read", "refs": []string{saved.Ref}})
	result, err = pipeline.Execute(t.Context(), call)
	if err != nil || !bytes.Contains(result.Data, []byte(`"claim":"The user states a preference for concise responses."`)) ||
		!bytes.Contains(result.Data, []byte(`"adopted":false`)) || approver.calls != 1 {
		t.Fatalf("read binding result: %s %v", result.Data, err)
	}
	call.CallID = "forged"
	call.Arguments = json.RawMessage(`{"operation":"count","session_id":"foreign"}`)
	if _, err := pipeline.Execute(t.Context(), call); err == nil {
		t.Fatal("schema permitted model-controlled trusted identity")
	}
	unknownCalls := 0
	unknownBinding := bindings[1]
	unknownBinding.Descriptor.Revision = "uncertain-test"
	unknownBinding.Executor = memoryExecutor(func(context.Context, experience.MemoryCaller, json.RawMessage) (json.RawMessage, error) {
		unknownCalls++
		return nil, atebridge.ErrOutcomeUnknown
	}, true)
	if _, err := registry.Activate(unknownBinding); err != nil {
		t.Fatal(err)
	}
	call.CallID, call.ToolName = "unknown", "memory_manage"
	call.Arguments = json.RawMessage(`{"operation":"withdraw_source","ref":"unused","reason":"test uncertain native response"}`)
	for range 2 {
		_, err := pipeline.Execute(t.Context(), call)
		var unknown *tool.OutcomeUnknownError
		var recoverable *tool.ExecutionError
		if !errors.As(err, &unknown) || errors.As(err, &recoverable) {
			t.Fatalf("uncertain native write became a retryable tool result: %v", err)
		}
	}
	if unknownCalls != 1 {
		t.Fatal("pipeline blindly repeated an uncertain memory mutation")
	}
}

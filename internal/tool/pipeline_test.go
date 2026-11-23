package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
)

func TestPipelineExecutesAllowedToolAndRecordsBoundaries(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	if _, err := registry.Activate(testBinding("rev-1", "ok")); err != nil {
		t.Fatal(err)
	}
	engine, err := policy.DefaultDeny(policy.Rule{
		ID:                "allow-pure-tools",
		ToolPattern:       "echo",
		CapabilityPattern: "tool.invoke",
		Effect:            policy.EffectAllow,
	})
	if err != nil {
		t.Fatal(err)
	}
	events := memorystore.New()
	pipeline := mustPipeline(t, registry, engine, nil, events)

	result, err := pipeline.Execute(context.Background(), Call{
		SessionID: "session-1",
		ToolName:  "echo",
		Arguments: json.RawMessage(`{"text":"hello"}`),
		Registry:  registry.Snapshot(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Revision != "rev-1" || !json.Valid(result.Data) {
		t.Fatalf("unexpected result: %#v", result)
	}

	recorded, err := events.Load(context.Background(), "session-1", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	wantTypes := []session.EventType{
		session.EventToolProposed,
		session.EventPolicyDecided,
		session.EventToolStarted,
		session.EventToolSucceeded,
	}
	if len(recorded) != len(wantTypes) {
		t.Fatalf("recorded %d events, want %d", len(recorded), len(wantTypes))
	}
	for index, want := range wantTypes {
		if recorded[index].Type != want {
			t.Fatalf("event %d = %q, want %q", index, recorded[index].Type, want)
		}
	}
}

func TestPipelineEmitsLiveLifecycleAfterDurableBoundaries(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	if _, err := registry.Activate(testBinding("rev-1", "ok")); err != nil {
		t.Fatal(err)
	}
	engine, err := policy.DefaultDeny(policy.Rule{
		ID: "allow-pure-tools", ToolPattern: "echo",
		CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow,
	})
	if err != nil {
		t.Fatal(err)
	}
	pipeline := mustPipeline(t, registry, engine, nil, memorystore.New())
	var live []LiveEventType
	ctx := WithLiveEventSink(context.Background(), func(_ context.Context, event LiveEvent) error {
		live = append(live, event.Type)
		return nil
	})
	if _, err := pipeline.Execute(ctx, Call{
		SessionID: "live-session", TurnID: "live-turn", CallID: "live-call",
		ToolName: "echo", Arguments: json.RawMessage(`{"text":"hello"}`),
	}); err != nil {
		t.Fatal(err)
	}
	want := []LiveEventType{
		LiveToolProposed,
		LivePolicyChecking,
		LivePolicyDecided,
		LiveToolStarted,
		LiveToolSucceeded,
	}
	if !slices.Equal(live, want) {
		t.Fatalf("live lifecycle = %#v, want %#v", live, want)
	}
}

func TestPipelineEmitsApprovalLifecycle(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	binding := testBinding("rev-1", "changed")
	binding.Descriptor.Name = "workspace_edit"
	binding.Descriptor.RiskClass = RiskWrite
	if _, err := registry.Activate(binding); err != nil {
		t.Fatal(err)
	}
	engine, err := policy.DefaultDeny(policy.Rule{
		ID: "approval", ToolPattern: "workspace_edit",
		CapabilityPattern: "tool.invoke", Effect: policy.EffectRequireApproval,
	})
	if err != nil {
		t.Fatal(err)
	}
	pipeline := mustPipeline(t, registry, engine, &testApprover{approved: true}, memorystore.New())
	var live []LiveEventType
	ctx := WithLiveEventSink(context.Background(), func(_ context.Context, event LiveEvent) error {
		live = append(live, event.Type)
		return nil
	})
	if _, err := pipeline.Execute(ctx, Call{
		SessionID: "approval-session", TurnID: "approval-turn", CallID: "approval-call",
		ToolName: "workspace_edit", Arguments: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	want := []LiveEventType{
		LiveToolProposed,
		LivePolicyChecking,
		LivePolicyDecided,
		LiveApprovalRequested,
		LiveApprovalWaiting,
		LiveApprovalResolved,
		LiveToolStarted,
		LiveToolSucceeded,
	}
	if !slices.Equal(live, want) {
		t.Fatalf("approval lifecycle = %#v, want %#v", live, want)
	}
}

func TestPipelineCarriesActorScopeToPolicyAndAudit(t *testing.T) {
	t.Parallel()
	registry := NewRegistry()
	if _, err := registry.Activate(testBinding("rev-1", "echo")); err != nil {
		t.Fatal(err)
	}
	engine := &recordingPolicyEngine{}
	events := memorystore.New()
	pipeline := mustPipeline(t, registry, engine, nil, events)
	teamID, _ := orchestration.NewTeamID()
	agentID, _ := orchestration.NewAgentID()
	threadID, _ := orchestration.NewThreadID()
	actor := orchestration.ActorScope{TeamID: teamID, AgentID: agentID, ThreadID: threadID}
	if _, err := pipeline.Execute(context.Background(), Call{
		Actor: actor, SessionID: "session-1", ToolName: "echo", Arguments: json.RawMessage(`{"text":"hello"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if engine.request.Actor != actor {
		t.Fatalf("policy actor = %#v, want %#v", engine.request.Actor, actor)
	}
	recorded, err := events.Load(context.Background(), "session-1", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(recorded[0].Payload, []byte(`"actor"`)) {
		t.Fatalf("proposal payload omitted actor: %s", recorded[0].Payload)
	}
}

func TestPipelineDoesNotInvokeDeniedTool(t *testing.T) {
	t.Parallel()

	invoked := false
	registry := NewRegistry()
	binding := testBinding("rev-1", "never")
	binding.Executor = InvokeFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
		invoked = true
		return json.RawMessage(`{}`), nil
	})
	if _, err := registry.Activate(binding); err != nil {
		t.Fatal(err)
	}
	engine, err := policy.DefaultDeny()
	if err != nil {
		t.Fatal(err)
	}
	events := memorystore.New()
	pipeline := mustPipeline(t, registry, engine, nil, events)

	_, err = pipeline.Execute(context.Background(), Call{
		SessionID: "session-1",
		ToolName:  "echo",
		Arguments: json.RawMessage(`{}`),
	})
	if !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("got %v, want ErrPolicyDenied", err)
	}
	if invoked {
		t.Fatal("denied executor was invoked")
	}
}

func TestPipelineRequiresAndResolvesApproval(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	invoked := false
	binding := testBinding("rev-1", "changed")
	binding.Descriptor.Name = "workspace_edit"
	binding.Descriptor.RiskClass = RiskWrite
	binding.Executor = InvokeFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
		invoked = true
		return json.RawMessage(`{"ok":true}`), nil
	})
	if _, err := registry.Activate(binding); err != nil {
		t.Fatal(err)
	}
	engine, err := policy.DefaultDeny(policy.Rule{ID: "approval", ToolPattern: "workspace_edit", CapabilityPattern: "tool.invoke", Effect: policy.EffectRequireApproval})
	if err != nil {
		t.Fatal(err)
	}
	approver := &testApprover{approved: true}
	pipeline := mustPipeline(t, registry, engine, approver, memorystore.New())
	result, err := pipeline.Execute(context.Background(), Call{SessionID: "session-1", ToolName: "workspace_edit", Arguments: json.RawMessage(`{}`)})
	if err != nil || !invoked || string(result.Data) != `{"ok":true}` || !approver.called {
		t.Fatalf("result=%s err=%v invoked=%v approver=%v", result.Data, err, invoked, approver.called)
	}
}

func TestNewPipelineValidatesTrustedDependencies(t *testing.T) {
	t.Parallel()

	if _, err := NewPipeline(PipelineConfig{}); err == nil {
		t.Fatal("NewPipeline accepted incomplete dependencies")
	}

	registry := NewRegistry()
	engine, err := policy.DefaultDeny()
	if err != nil {
		t.Fatal(err)
	}
	events := memorystore.New()
	pipeline, err := NewPipeline(PipelineConfig{Registry: registry, Policy: engine, Events: events})
	if err != nil {
		t.Fatal(err)
	}
	if pipeline == nil {
		t.Fatal("NewPipeline returned nil pipeline")
	}
}

func TestPipelineWithApproverDoesNotMutateOriginal(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	engine, err := policy.DefaultDeny()
	if err != nil {
		t.Fatal(err)
	}
	events := memorystore.New()
	original, err := NewPipeline(PipelineConfig{Registry: registry, Policy: engine, Events: events})
	if err != nil {
		t.Fatal(err)
	}
	approver := &testApprover{approved: true}
	derived := original.WithApprover(approver)
	if derived == original || derived == nil {
		t.Fatal("WithApprover did not create an independent view")
	}
	if derived.approver != approver || original.approver != nil {
		t.Fatal("WithApprover mutated the original pipeline")
	}
}

func TestPipelineRejectsForeignRegistrySnapshot(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	foreign := NewRegistry()
	if _, err := foreign.Activate(testBinding("rev-1", "foreign")); err != nil {
		t.Fatal(err)
	}
	engine, err := policy.DefaultDeny(policy.Rule{
		ID:                "allow-echo",
		ToolPattern:       "echo",
		CapabilityPattern: "tool.invoke",
		Effect:            policy.EffectAllow,
	})
	if err != nil {
		t.Fatal(err)
	}
	pipeline := mustPipeline(t, registry, engine, nil, memorystore.New())
	_, err = pipeline.Execute(context.Background(), Call{
		SessionID: "session-1",
		ToolName:  "echo",
		Arguments: json.RawMessage(`{"text":"hello"}`),
		Registry:  foreign.Snapshot(),
	})
	if !errors.Is(err, ErrForeignSnapshot) {
		t.Fatalf("got %v, want ErrForeignSnapshot", err)
	}
}

func TestPipelineReplaysDurableSuccessfulCall(t *testing.T) {
	t.Parallel()
	registry := NewRegistry()
	var invocations atomic.Int32
	binding := testBinding("rev-1", "ok")
	binding.Executor = InvokeFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
		invocations.Add(1)
		return json.RawMessage(`{"ok":true}`), nil
	})
	if _, err := registry.Activate(binding); err != nil {
		t.Fatal(err)
	}
	engine, err := policy.DefaultDeny(policy.Rule{ID: "allow", ToolPattern: "echo", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow})
	if err != nil {
		t.Fatal(err)
	}
	events := memorystore.New()
	pipeline := mustPipeline(t, registry, engine, nil, events)
	call := Call{SessionID: "session-replay", TurnID: "turn-1", CallID: "stable-call", ToolName: "echo", Arguments: json.RawMessage(`{"text":"same"}`)}
	first, err := pipeline.Execute(context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}
	second, err := pipeline.Execute(context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}
	if invocations.Load() != 1 || !bytes.Equal(first.Data, second.Data) {
		t.Fatalf("invocations=%d first=%s second=%s", invocations.Load(), first.Data, second.Data)
	}
	recorded, err := events.Load(context.Background(), call.SessionID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(recorded) != 4 {
		t.Fatalf("replay appended events: got %d, want 4", len(recorded))
	}
	call.Arguments = json.RawMessage(`{"text":"changed"}`)
	if _, err := pipeline.Execute(context.Background(), call); !errors.Is(err, ErrCallConflict) {
		t.Fatalf("conflicting call error = %v, want ErrCallConflict", err)
	}
	call.TurnID = "turn-2"
	if _, err := pipeline.Execute(context.Background(), call); err != nil {
		t.Fatalf("same provider call ID in another turn was treated as a replay: %v", err)
	}
	if invocations.Load() != 2 {
		t.Fatalf("cross-turn invocations = %d, want 2", invocations.Load())
	}
}

func mustPipeline(t *testing.T, registry *Registry, engine policy.Engine, approver policy.Approver, events session.EventStore) *Pipeline {
	t.Helper()
	pipeline, err := NewPipeline(PipelineConfig{Registry: registry, Policy: engine, Approver: approver, Events: events})
	if err != nil {
		t.Fatal(err)
	}
	return pipeline
}

type testApprover struct {
	approved bool
	called   bool
}

type recordingPolicyEngine struct {
	request policy.Request
}

func (engine *recordingPolicyEngine) Evaluate(_ context.Context, request policy.Request) (policy.Decision, error) {
	engine.request = request
	return policy.Decision{Effect: policy.EffectAllow, RuleID: "test"}, nil
}

func (approver *testApprover) Approve(context.Context, policy.Request, policy.Decision) (bool, error) {
	approver.called = true
	return approver.approved, nil
}

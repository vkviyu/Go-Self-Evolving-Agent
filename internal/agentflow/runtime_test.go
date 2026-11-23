package agentflow_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentflow"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/testfixture"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func TestParallelStreamsIntoDownstreamBeforeSlowBranchCompletes(t *testing.T) {
	t.Parallel()
	harness := newFlowHarness(t, nil)
	observed := make(chan string, 2)
	fastObserved := make(chan struct{})
	definition := agentflow.Pipeline("streaming", "v1",
		agentflow.Parallel("fanout",
			agentflow.Map("fast", func(ctx context.Context, input agentflow.Value) (agentflow.Value, error) {
				return input, nil
			}),
			agentflow.Map("slow", func(ctx context.Context, input agentflow.Value) (agentflow.Value, error) {
				// This branch can complete only after downstream consumes
				// the fast result. A join therefore cannot satisfy the test.
				select {
				case <-fastObserved:
					return input, nil
				case <-ctx.Done():
					return agentflow.Value{}, ctx.Err()
				}
			}),
		),
		agentflow.Each("observe", 2, agentflow.Map("record", func(ctx context.Context, input agentflow.Value) (agentflow.Value, error) {
			observed <- input.Key
			if input.Key == "fast" {
				close(fastObserved)
			}
			return input, nil
		})),
	)
	input, _ := agentflow.NewValue(map[string]any{"task": "test"})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	result, err := harness.runtime.Run(ctx, definition, harness.invocation("streaming-1", input))
	if err != nil {
		t.Fatal(err)
	}
	first := <-observed
	if first != "fast" {
		t.Fatalf("first downstream result = %q; parallel stage behaved like a join", first)
	}
	if len(result.Outputs) != 2 {
		t.Fatalf("outputs = %d, want 2", len(result.Outputs))
	}
}

func TestDefinitionRejectsMissingRevisionAndDuplicateBranches(t *testing.T) {
	t.Parallel()
	identity := agentflow.Map("same", func(_ context.Context, input agentflow.Value) (agentflow.Value, error) { return input, nil })
	if err := agentflow.Pipeline("missing_revision", "", agentflow.Each("work", 1, identity)).Validate(); err == nil {
		t.Fatal("definition without revision was accepted")
	}
	if err := agentflow.Pipeline("duplicate_branches", "v1", agentflow.Parallel("fanout", identity, identity)).Validate(); err == nil {
		t.Fatal("parallel stage with duplicate branch names was accepted")
	}
}

func TestCompactDropsFailedAndEmptyBranches(t *testing.T) {
	t.Parallel()
	harness := newFlowHarness(t, nil)
	failed := agentflow.ProcessorFunc{ProcessorName: "failed", Function: func(context.Context, *agentflow.Execution, string, agentflow.Value) (agentflow.Value, error) {
		value, _ := agentflow.NewValue("diagnostic from failed branch")
		return value, errors.New("branch failed")
	}}
	empty := agentflow.Map("empty", func(context.Context, agentflow.Value) (agentflow.Value, error) {
		return agentflow.NewValue(nil)
	})
	valid := agentflow.Map("valid", func(context.Context, agentflow.Value) (agentflow.Value, error) {
		return agentflow.NewValue("usable")
	})
	definition := agentflow.Pipeline("compact", "v1", agentflow.Parallel("fanout", failed, empty, valid), agentflow.Compact("usable", nil))
	input, _ := agentflow.NewValue("input")
	result, err := harness.runtime.Run(context.Background(), definition, harness.invocation("compact-1", input))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Outputs) != 1 || string(result.Outputs[0].Data) != `"usable"` {
		t.Fatalf("outputs = %#v, want only usable branch", result.Outputs)
	}
}

func TestFlowDoesNotCollapseEqualStreamItemsIntoOneDurableStep(t *testing.T) {
	t.Parallel()
	harness := newFlowHarness(t, nil)
	var processed atomic.Int32
	processor := agentflow.Map("count", func(_ context.Context, input agentflow.Value) (agentflow.Value, error) {
		processed.Add(1)
		return input, nil
	})
	definition := agentflow.Pipeline("duplicate-items", "v1", duplicateItemsStage{}, agentflow.Each("work", 2, processor))
	input, _ := agentflow.NewValue(map[string]any{"same": true})
	result, err := harness.runtime.Run(context.Background(), definition, harness.invocation("duplicate-items-1", input))
	if err != nil {
		t.Fatal(err)
	}
	if processed.Load() != 2 || len(result.Outputs) != 2 {
		t.Fatalf("processed=%d outputs=%d, want two independent items", processed.Load(), len(result.Outputs))
	}
}

func TestFlowParentCancellationCannotBecomeCompleted(t *testing.T) {
	t.Parallel()
	harness := newFlowHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel only after durable admission and delivery of a partial output. A
	// wall-clock delay can fire during CreateRun under race instrumentation and
	// would test pre-admission cancellation instead of quiet stage shutdown.
	definition := agentflow.Pipeline("quiet-cancel", "v1", quietUntilCanceledStage{cancelParent: cancel})
	input, _ := agentflow.NewValue("partial")
	result, err := harness.runtime.Run(ctx, definition, harness.invocation("quiet-cancel-1", input))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v, want context canceled", err)
	}
	if result.Run.State != agentflow.StateCanceled {
		t.Fatalf("canceled Flow state = %s, want canceled", result.Run.State)
	}
}

func TestFlowCanceledBeforeAdmissionDoesNotExecute(t *testing.T) {
	t.Parallel()
	harness := newFlowHarness(t, nil)
	var processed atomic.Int32
	definition := agentflow.Pipeline("pre-admission-cancel", "v1", agentflow.Each("work", 1, agentflow.Map("count", func(_ context.Context, input agentflow.Value) (agentflow.Value, error) {
		processed.Add(1)
		return input, nil
	})))
	input, _ := agentflow.NewValue("input")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := harness.runtime.Run(ctx, definition, harness.invocation("pre-admission-cancel-1", input))
	if !errors.Is(err, context.Canceled) || result.Run.ID != "" || processed.Load() != 0 {
		t.Fatalf("pre-admission cancellation: run=%+v processed=%d err=%v", result.Run, processed.Load(), err)
	}
}

func TestFlowRejectsInvalidCustomStageOutput(t *testing.T) {
	t.Parallel()
	harness := newFlowHarness(t, nil)
	definition := agentflow.Pipeline("invalid-output", "v1", invalidFinalStage{})
	input, _ := agentflow.NewValue("input")
	result, err := harness.runtime.Run(context.Background(), definition, harness.invocation("invalid-output-1", input))
	if err == nil {
		t.Fatal("invalid custom stage output was accepted")
	}
	if result.Run.State != agentflow.StateFailed {
		t.Fatalf("invalid output Flow state = %s, want failed", result.Run.State)
	}
}

func TestConcurrentIdempotentFlowInvocationsExecuteOnce(t *testing.T) {
	t.Parallel()
	harness := newFlowHarness(t, nil)
	var processed atomic.Int32
	definition := agentflow.Pipeline("concurrent-idempotent", "v1", agentflow.Each("work", 1, agentflow.Map("count", func(_ context.Context, input agentflow.Value) (agentflow.Value, error) {
		processed.Add(1)
		time.Sleep(40 * time.Millisecond)
		return input, nil
	})))
	input, _ := agentflow.NewValue("input")
	invocation := harness.invocation("concurrent-idempotent-1", input)
	results := make(chan struct {
		result agentflow.Result
		err    error
	}, 2)
	for range 2 {
		go func() {
			result, err := harness.runtime.Run(context.Background(), definition, invocation)
			results <- struct {
				result agentflow.Result
				err    error
			}{result, err}
		}()
	}
	first := <-results
	second := <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent Flow errors: %v, %v", first.err, second.err)
	}
	if processed.Load() != 1 {
		t.Fatalf("processor executions = %d, want one", processed.Load())
	}
	if first.result.Run.ID != second.result.Run.ID || first.result.Run.State != agentflow.StateCompleted || second.result.Run.State != agentflow.StateCompleted {
		t.Fatalf("concurrent results = %#v and %#v, want same completed run", first.result.Run, second.result.Run)
	}
}

func TestSuspendedFlowStaysRunningAndReplaysCompletedToolCall(t *testing.T) {
	t.Parallel()
	var sideEffects atomic.Int32
	binding := testFlowBinding("side_effect", tool.RiskWrite, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		sideEffects.Add(1)
		return json.RawMessage(`{"ok":true}`), nil
	})
	harness := newFlowHarness(t, []bindingRule{{binding: binding, effect: policy.EffectAllow}})
	var attempts atomic.Int32
	processor := agentflow.ProcessorFunc{ProcessorName: "action", Function: func(ctx context.Context, execution *agentflow.Execution, stepID string, input agentflow.Value) (agentflow.Value, error) {
		data, err := execution.Tool(ctx, stepID+".write", "side_effect", map[string]any{"value": 1})
		if err != nil {
			return agentflow.Value{}, err
		}
		input.Data = data
		if attempts.Add(1) == 1 {
			return input, &orchestration.WaitSuspendedError{}
		}
		return input, nil
	}}
	definition := agentflow.Pipeline("recoverable", "v1", agentflow.Each("work", 1, processor))
	input, _ := agentflow.NewValue("input")
	invocation := harness.invocation("recoverable-1", input)
	first, err := harness.runtime.Run(context.Background(), definition, invocation)
	var suspended *orchestration.WaitSuspendedError
	if !errors.As(err, &suspended) || first.Run.State != agentflow.StateRunning {
		t.Fatalf("first run state=%q err=%v, want running suspension", first.Run.State, err)
	}
	second, err := harness.runtime.Run(context.Background(), definition, invocation)
	if err != nil {
		t.Fatal(err)
	}
	if second.Run.ID != first.Run.ID || second.Run.State != agentflow.StateCompleted {
		t.Fatalf("resumed run = %#v, first ID %s", second.Run, first.Run.ID)
	}
	if sideEffects.Load() != 1 {
		t.Fatalf("side effects = %d, want durable replay without duplication", sideEffects.Load())
	}
}

func TestFlowRunRejectsRevisionAndTopologyDrift(t *testing.T) {
	t.Parallel()
	harness := newFlowHarness(t, nil)
	identity := func(_ context.Context, input agentflow.Value) (agentflow.Value, error) { return input, nil }
	input, _ := agentflow.NewValue("input")
	invocation := harness.invocation("definition-drift-1", input)
	original := agentflow.Pipeline("versioned", "v1", agentflow.Each("work", 1, agentflow.Map("identity", identity)))
	if _, err := harness.runtime.Run(context.Background(), original, invocation); err != nil {
		t.Fatal(err)
	}
	revised := agentflow.Pipeline("versioned", "v2", agentflow.Each("work", 1, agentflow.Map("identity", identity)))
	if _, err := harness.runtime.Run(context.Background(), revised, invocation); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("revision drift error = %v, want idempotency conflict", err)
	}
	rewired := agentflow.Pipeline("versioned", "v1", agentflow.Each("renamed", 1, agentflow.Map("identity", identity)))
	if _, err := harness.runtime.Run(context.Background(), rewired, invocation); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("topology drift error = %v, want idempotency conflict", err)
	}
}

func TestAgentSessionReusesChildAcrossStages(t *testing.T) {
	t.Parallel()
	var spawns, continuations, waits atomic.Int32
	agentID := orchestration.AgentID("agent-flow-session")
	spawn := testFlowBinding("agent_spawn", tool.RiskWrite, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		spawns.Add(1)
		return json.Marshal(map[string]any{"agent_id": agentID})
	})
	continueBinding := testFlowBinding("agent_continue", tool.RiskWrite, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		continuations.Add(1)
		return json.Marshal(map[string]any{"agent_id": agentID})
	})
	wait := testFlowBinding("agent_wait", tool.RiskRead, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		waits.Add(1)
		return json.Marshal(map[string]any{
			"agents":    []orchestration.AgentSnapshot{{ID: agentID, Result: &orchestration.AgentResult{Status: orchestration.ResultSucceeded, Summary: "stage complete"}}},
			"satisfied": true, "timed_out": false,
		})
	})
	harness := newFlowHarness(t, []bindingRule{
		{binding: spawn, effect: policy.EffectAllow},
		{binding: continueBinding, effect: policy.EffectAllow},
		{binding: wait, effect: policy.EffectAllow},
	})
	// flow_agent_sessions enforces an Agent foreign key. Reuse the root row as a
	// deterministic test fixture; the fake tool boundary still verifies calls.
	agentID = harness.root.AgentID
	sessionProcessor := agentflow.AgentSession(agentflow.AgentSessionConfig{
		Name: "worker", Prompt: func(input agentflow.Value) (string, error) { return "process stage", nil },
	})
	definition := agentflow.Pipeline("stateful_session", "v1",
		agentflow.Each("first", 1, sessionProcessor),
		agentflow.Each("second", 1, sessionProcessor),
	)
	input, _ := agentflow.NewValue(map[string]any{"task": "two stages"})
	result, err := harness.runtime.Run(context.Background(), definition, harness.invocation("stateful-session-1", input))
	if err != nil {
		t.Fatal(err)
	}
	if result.Run.State != agentflow.StateCompleted || spawns.Load() != 1 || continuations.Load() != 1 || waits.Load() != 2 {
		t.Fatalf("state=%s spawn=%d continue=%d wait=%d", result.Run.State, spawns.Load(), continuations.Load(), waits.Load())
	}
}

func TestAgentSessionKeepsDuplicateItemsInSeparateChildSessions(t *testing.T) {
	t.Parallel()
	var spawns, waits atomic.Int32
	agentID := orchestration.AgentID("agent-duplicate-session")
	spawn := testFlowBinding("agent_spawn", tool.RiskWrite, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		spawns.Add(1)
		return json.Marshal(map[string]any{"agent_id": agentID})
	})
	wait := testFlowBinding("agent_wait", tool.RiskRead, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		waits.Add(1)
		return json.Marshal(map[string]any{
			"agents":    []orchestration.AgentSnapshot{{ID: agentID, Result: &orchestration.AgentResult{Status: orchestration.ResultSucceeded, Summary: "done"}}},
			"satisfied": true, "timed_out": false,
		})
	})
	harness := newFlowHarness(t, []bindingRule{
		{binding: spawn, effect: policy.EffectAllow},
		{binding: wait, effect: policy.EffectAllow},
	})
	agentID = harness.root.AgentID
	definition := agentflow.Pipeline("duplicate-agent-sessions", "v1",
		duplicateItemsStage{},
		agentflow.Each("work", 2, agentflow.AgentSession(agentflow.AgentSessionConfig{
			Name: "worker", Prompt: func(agentflow.Value) (string, error) { return "work", nil },
		})),
	)
	input, _ := agentflow.NewValue(map[string]any{"same": true})
	result, err := harness.runtime.Run(context.Background(), definition, harness.invocation("duplicate-agent-sessions-1", input))
	if err != nil {
		t.Fatal(err)
	}
	if result.Run.State != agentflow.StateCompleted || spawns.Load() != 2 || waits.Load() != 2 || len(result.Outputs) != 2 {
		t.Fatalf("state=%s spawn=%d wait=%d outputs=%d, want two independent sessions", result.Run.State, spawns.Load(), waits.Load(), len(result.Outputs))
	}
}

func TestAgentSessionResumeDoesNotTurnSpawnIntoContinuation(t *testing.T) {
	t.Parallel()
	var spawns, continuations, waits atomic.Int32
	agentID := orchestration.AgentID("agent-placeholder")
	spawn := testFlowBinding("agent_spawn", tool.RiskWrite, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		spawns.Add(1)
		return json.Marshal(map[string]any{"agent_id": agentID})
	})
	continueBinding := testFlowBinding("agent_continue", tool.RiskWrite, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		continuations.Add(1)
		return json.Marshal(map[string]any{"agent_id": agentID})
	})
	wait := testFlowBinding("agent_wait", tool.RiskRead, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		if waits.Add(1) == 1 {
			return nil, &orchestration.WaitSuspendedError{}
		}
		return json.Marshal(map[string]any{
			"agents":    []orchestration.AgentSnapshot{{ID: agentID, Result: &orchestration.AgentResult{Status: orchestration.ResultSucceeded, Summary: "done"}}},
			"satisfied": true, "timed_out": false,
		})
	})
	harness := newFlowHarness(t, []bindingRule{
		{binding: spawn, effect: policy.EffectAllow},
		{binding: continueBinding, effect: policy.EffectAllow},
		{binding: wait, effect: policy.EffectAllow},
	})
	agentID = harness.root.AgentID
	definition := agentflow.Pipeline("session_resume", "v1", agentflow.Each("work", 1, agentflow.AgentSession(agentflow.AgentSessionConfig{
		Name: "worker", Prompt: func(agentflow.Value) (string, error) { return "work", nil },
	})))
	input, _ := agentflow.NewValue("input")
	invocation := harness.invocation("session-resume-1", input)
	first, err := harness.runtime.Run(context.Background(), definition, invocation)
	var suspended *orchestration.WaitSuspendedError
	if !errors.As(err, &suspended) || first.Run.State != agentflow.StateRunning {
		t.Fatalf("first result=%#v err=%v", first, err)
	}
	if _, err := harness.runtime.Run(context.Background(), definition, invocation); err != nil {
		t.Fatal(err)
	}
	if spawns.Load() != 1 || continuations.Load() != 0 || waits.Load() != 2 {
		t.Fatalf("spawn=%d continue=%d wait=%d", spawns.Load(), continuations.Load(), waits.Load())
	}
}

func TestCatalogRunsNamedFlowAndInheritsCallerApproval(t *testing.T) {
	t.Parallel()
	var writes atomic.Int32
	binding := testFlowBinding("protected_write", tool.RiskWrite, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		writes.Add(1)
		return json.RawMessage(`{"written":true}`), nil
	})
	harness := newFlowHarness(t, []bindingRule{
		{binding: binding, effect: policy.EffectRequireApproval},
		{toolName: agentflow.RunToolName, effect: policy.EffectAllow},
		{toolName: agentflow.ListToolName, effect: policy.EffectAllow},
	})
	catalog, err := agentflow.NewCatalog(harness.runtime)
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Register(
		agentflow.Registration{Definition: agentflow.Pipeline("write_flow", "v1", agentflow.Each("write", 1, agentflow.Tool("protected", "protected_write", func(agentflow.Value) (any, error) {
			return map[string]any{"value": 1}, nil
		}))), Description: "Exercise a protected write."},
		agentflow.Registration{Definition: agentflow.Pipeline("pure_flow", "v1", agentflow.Each("copy", 1, agentflow.Map("identity", func(_ context.Context, input agentflow.Value) (agentflow.Value, error) {
			return input, nil
		}))), Description: "Return the input unchanged."},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.registry.ActivateMany(agentflow.CatalogBindings(catalog)...); err != nil {
		t.Fatal(err)
	}
	deniedPipeline := harness.pipeline.WithApprover(staticApprover(false))
	_, err = deniedPipeline.Execute(context.Background(), tool.Call{
		Actor: harness.root.ActorScope(), SessionID: harness.root.SessionID, CallID: "catalog-write",
		ToolName: agentflow.RunToolName, Arguments: json.RawMessage(`{"name":"write_flow","input":{"task":"write"},"idempotency_key":"write-flow-1"}`),
	})
	if !errors.Is(err, tool.ErrApprovalDenied) || writes.Load() != 0 {
		t.Fatalf("protected Flow err=%v writes=%d, caller approval was not inherited", err, writes.Load())
	}
	result, err := deniedPipeline.Execute(context.Background(), tool.Call{
		Actor: harness.root.ActorScope(), SessionID: harness.root.SessionID, CallID: "catalog-pure",
		ToolName: agentflow.RunToolName, Arguments: json.RawMessage(`{"name":"pure_flow","input":{"task":"copy"},"idempotency_key":"pure-flow-1"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		State   agentflow.State   `json:"state"`
		Outputs []agentflow.Value `json:"outputs"`
	}
	if err := json.Unmarshal(result.Data, &response); err != nil {
		t.Fatal(err)
	}
	if response.State != agentflow.StateCompleted || len(response.Outputs) != 1 {
		t.Fatalf("named Flow response = %s", result.Data)
	}
	descriptors := catalog.List()
	if len(descriptors) != 2 || descriptors[0].Name != "pure_flow" || descriptors[1].Name != "write_flow" {
		t.Fatalf("catalog descriptors = %#v", descriptors)
	}
	if descriptors[0].Revision != "v1" || len(descriptors[0].Fingerprint) != 64 {
		t.Fatalf("catalog descriptor lacks durable identity: %#v", descriptors[0])
	}
}

type bindingRule struct {
	binding  tool.Binding
	toolName string
	effect   policy.Effect
}

type flowHarness struct {
	runtime  *agentflow.Runtime
	registry *tool.Registry
	pipeline *tool.Pipeline
	root     orchestration.RootThread
}

func newFlowHarness(t *testing.T, rules []bindingRule) flowHarness {
	t.Helper()
	database, err := testfixture.Open(t, context.Background(), filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	root, _, err := database.Orchestration().EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: session.ID("flow-test-session")})
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry()
	bindings := make([]tool.Binding, 0, len(rules))
	policyRules := make([]policy.Rule, 0, len(rules))
	for index, rule := range rules {
		name := rule.toolName
		if rule.binding.Descriptor.Name != "" {
			name = rule.binding.Descriptor.Name
			bindings = append(bindings, rule.binding)
		}
		policyRules = append(policyRules, policy.Rule{
			ID: "flow-test-rule-" + string(rune('a'+index)), ToolPattern: name,
			CapabilityPattern: "tool.invoke", Effect: rule.effect,
		})
	}
	if len(bindings) > 0 {
		if _, err := registry.ActivateMany(bindings...); err != nil {
			t.Fatal(err)
		}
	}
	engine, err := policy.DefaultDeny(policyRules...)
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := tool.NewPipeline(tool.PipelineConfig{Registry: registry, Policy: engine, Approver: staticApprover(true), Events: database.SessionEvents()})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := agentflow.NewRuntime(agentflow.RuntimeConfig{Pipeline: pipeline, Registry: registry, Store: database.Orchestration()})
	if err != nil {
		t.Fatal(err)
	}
	return flowHarness{runtime: runtime, registry: registry, pipeline: pipeline, root: root}
}

func (harness flowHarness) invocation(key string, input agentflow.Value) agentflow.Invocation {
	return agentflow.Invocation{Actor: harness.root.ActorScope(), SessionID: harness.root.SessionID, IdempotencyKey: key, Input: input}
}

func testFlowBinding(name string, risk tool.RiskClass, invoke tool.InvokeFunc) tool.Binding {
	return tool.Binding{Descriptor: tool.Descriptor{
		Name: name, Description: name, InputSchema: json.RawMessage(`{"type":"object"}`), Revision: "test-v1", RiskClass: risk,
	}, Executor: invoke}
}

type staticApprover bool

func (approver staticApprover) Approve(context.Context, policy.Request, policy.Decision) (bool, error) {
	return bool(approver), nil
}

// duplicateItemsStage models a fan-out source that emits equal payloads. The
// runtime must preserve their item identity instead of treating the second
// value as a replay of the first durable step.
type duplicateItemsStage struct{}

func (duplicateItemsStage) Name() string { return "duplicate-source" }

func (duplicateItemsStage) Apply(_ context.Context, _ *agentflow.Execution, input agentflow.Stream) agentflow.Stream {
	output := make(chan agentflow.Item, 2)
	value := <-input
	output <- value
	output <- value
	close(output)
	return output
}

type quietUntilCanceledStage struct{ cancelParent context.CancelFunc }

func (quietUntilCanceledStage) Name() string { return "quiet-cancel-source" }

func (stage quietUntilCanceledStage) Apply(ctx context.Context, _ *agentflow.Execution, input agentflow.Stream) agentflow.Stream {
	output := make(chan agentflow.Item)
	go func() {
		defer close(output)
		item, ok := <-input
		if !ok {
			return
		}
		select {
		case output <- item:
			stage.cancelParent()
		case <-ctx.Done():
			return
		}
		<-ctx.Done()
	}()
	return output
}

type invalidFinalStage struct{}

func (invalidFinalStage) Name() string { return "invalid-final" }

func (invalidFinalStage) Apply(_ context.Context, _ *agentflow.Execution, _ agentflow.Stream) agentflow.Stream {
	output := make(chan agentflow.Item, 1)
	output <- agentflow.Item{Final: true}
	close(output)
	return output
}

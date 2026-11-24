package agentflow_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentflow"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func TestFlowSnapshotRecoveryReopensExactViewAndReplaysPriorWrite(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "flow.db")
	openAuthority := flowAuthorityOpener(t)
	var oldCalls, newCalls, attempts atomic.Int32
	oldBinding := testFlowBinding("write_once", tool.RiskWrite, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		oldCalls.Add(1)
		return json.RawMessage(`{"revision":"old"}`), nil
	})
	newBinding := oldBinding
	newBinding.Descriptor.Revision = "v2"
	newBinding.Executor = tool.InvokeFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
		newCalls.Add(1)
		return json.RawMessage(`{"revision":"new"}`), nil
	})
	open := func(bindings ...tool.Binding) (*runtimestore.Database, *tool.Registry, *agentflow.Runtime, orchestration.RootThread) {
		database, err := runtimestore.OpenRuntime(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = database.Close() })
		repositories, events := openAuthority(database)
		root, _, err := repositories.EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "flow-snapshot-session"})
		if err != nil {
			t.Fatal(err)
		}
		registry := tool.NewRegistry()
		if _, err := registry.ActivateMany(bindings...); err != nil {
			t.Fatal(err)
		}
		snapshots := database.RuntimeSnapshots().WithReferenceGuard(repositories.CheckSnapshotReference)
		if err := registry.ConfigurePersistence(snapshots); err != nil {
			t.Fatal(err)
		}
		engine, err := policy.DefaultDeny(policy.Rule{ID: "allow-fixture", ToolPattern: "write_once", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow})
		if err != nil {
			t.Fatal(err)
		}
		pipeline, err := tool.NewPipeline(tool.PipelineConfig{Registry: registry, Policy: engine, Events: events})
		if err != nil {
			t.Fatal(err)
		}
		runtime, err := agentflow.NewRuntime(agentflow.RuntimeConfig{Pipeline: pipeline, Registry: registry, Store: repositories})
		if err != nil {
			t.Fatal(err)
		}
		return database, registry, runtime, root
	}
	processor := agentflow.ProcessorFunc{ProcessorName: "write", Function: func(ctx context.Context, execution *agentflow.Execution, stepID string, input agentflow.Value) (agentflow.Value, error) {
		if _, err := execution.Tool(ctx, stepID+".first", "write_once", map[string]any{}); err != nil {
			return input, err
		}
		if attempts.Add(1) == 1 {
			return input, &orchestration.WaitSuspendedError{}
		}
		data, err := execution.Tool(ctx, stepID+".second", "write_once", map[string]any{})
		input.Data = data
		return input, err
	}}
	definition := agentflow.Pipeline("snapshot-recovery", "v1", agentflow.Each("work", 1, processor))
	database, _, runtime, root := open(oldBinding)
	input, _ := agentflow.NewValue("input")
	invocation := agentflow.Invocation{Actor: root.ActorScope(), SessionID: root.SessionID, IdempotencyKey: "same-flow", Input: input}
	first, err := runtime.Run(ctx, definition, invocation)
	var suspended *orchestration.WaitSuspendedError
	if !errors.As(err, &suspended) || first.Run.State != agentflow.StateRunning || oldCalls.Load() != 1 {
		t.Fatalf("first=%+v old=%d err=%v", first.Run, oldCalls.Load(), err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, _, runtime, _ = open(newBinding)
	// Omitting the original ID must not silently rebind an existing Flow.
	if _, err := runtime.Run(ctx, definition, invocation); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("Flow rebound to latest registry: %v", err)
	}
	invocation.RuntimeSnapshotID = first.Run.RuntimeSnapshotID
	if _, err := runtime.Run(ctx, definition, invocation); !errors.Is(err, tool.ErrSnapshotUnavailable) {
		t.Fatalf("incompatible tool revision accepted on restart: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	extra := newBinding
	extra.Descriptor.Name = "extra"
	database, registry, runtime, _ := open(oldBinding, extra)
	active := registry.Snapshot()
	second, err := runtime.Run(ctx, definition, invocation)
	if err != nil {
		t.Fatal(err)
	}
	if second.Run.ID != first.Run.ID || second.Run.State != agentflow.StateCompleted || len(second.Outputs) != 1 || string(second.Outputs[0].Data) != `{"revision":"old"}` {
		t.Fatalf("resumed=%+v", second)
	}
	if oldCalls.Load() != 2 || newCalls.Load() != 0 {
		t.Fatalf("replayed write or switched implementation: old=%d new=%d", oldCalls.Load(), newCalls.Load())
	}
	if registry.Snapshot() != active {
		t.Fatal("Flow recovery replaced active view")
	}
	refs, err := database.RuntimeSnapshots().References(ctx, first.Run.RuntimeSnapshotID)
	if err != nil || len(refs) != 1 || refs[0].Kind != "flow" || refs[0].ID != "same-flow" {
		t.Fatalf("refs=%+v err=%v", refs, err)
	}
}

type failingFlowSnapshots struct{}

func (failingFlowSnapshots) Save(context.Context, tool.SnapshotManifest, tool.SnapshotReference) error {
	return errors.New("snapshot store unavailable")
}
func (failingFlowSnapshots) Load(context.Context, string) (tool.SnapshotManifest, error) {
	return tool.SnapshotManifest{}, tool.ErrSnapshotUnavailable
}

func TestFlowCannotExecuteBeforeSnapshotIsRetained(t *testing.T) {
	harness := newFlowHarness(t, nil)
	if err := harness.registry.ConfigurePersistence(failingFlowSnapshots{}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	definition := agentflow.Pipeline("no-snapshot", "v1", agentflow.Each("work", 1, agentflow.Map("action", func(_ context.Context, value agentflow.Value) (agentflow.Value, error) {
		calls.Add(1)
		return value, nil
	})))
	input, _ := agentflow.NewValue("input")
	result, err := harness.runtime.Run(context.Background(), definition, harness.invocation("no-snapshot", input))
	if err == nil || calls.Load() != 0 || result.Run.ID != "" {
		t.Fatalf("executed without durable snapshot: result=%+v calls=%d err=%v", result, calls.Load(), err)
	}
}

package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentflow"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/sqlitestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func TestCatalogFlowInheritsProfileGrantAndSharedToolBudget(t *testing.T) {
	for _, scenario := range []string{"forbidden-nested-write", "nested-budget"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			database, err := sqlitestore.Open(ctx, ":memory:")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = database.Close() })
			root, _, err := database.Orchestration().EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "flow-profile", ProfileRevision: "worker-v1"})
			if err != nil {
				t.Fatal(err)
			}
			var reads, writes atomic.Int32
			read := profileBinding("read", tool.RiskRead, func(context.Context, json.RawMessage) (json.RawMessage, error) {
				reads.Add(1)
				return json.RawMessage(`{}`), nil
			})
			write := profileBinding("write", tool.RiskWrite, func(context.Context, json.RawMessage) (json.RawMessage, error) {
				writes.Add(1)
				return json.RawMessage(`{}`), nil
			})
			parent := profileFactory(t, memorystore.New(), &summaryModel{}, read, write)
			runtime, err := agentflow.NewRuntime(agentflow.RuntimeConfig{Registry: parent.registry, Pipeline: parent.pipeline, Store: database.Orchestration()})
			if err != nil {
				t.Fatal(err)
			}
			catalog, err := agentflow.NewCatalog(runtime)
			if err != nil {
				t.Fatal(err)
			}
			args := func(agentflow.Value) (any, error) { return map[string]any{}, nil }
			stages := []agentflow.Stage{agentflow.Each("first", 1, agentflow.Tool("write-step", "write", args))}
			if scenario == "nested-budget" {
				stages = []agentflow.Stage{agentflow.Each("first", 1, agentflow.Tool("read-one", "read", args)), agentflow.Each("second", 1, agentflow.Tool("read-two", "read", args))}
			}
			if err := catalog.Register(agentflow.Registration{Definition: agentflow.Pipeline("test-flow", "v1", stages...), Description: "Profile boundary test"}); err != nil {
				t.Fatal(err)
			}
			if _, err := parent.registry.ActivateMany(agentflow.CatalogBindings(catalog)...); err != nil {
				t.Fatal(err)
			}
			profile := runtimeTestProfile()
			profile.Tools, profile.Limits.MaxToolCalls = []string{"read", "flow_run"}, 2
			child, err := parent.WithProfile(ctx, root.ActorScope(), root.SessionID, profile)
			if err != nil {
				t.Fatal(err)
			}
			snapshot := parent.registry.Snapshot()
			identity, err := snapshot.ID()
			if err != nil {
				t.Fatal(err)
			}
			_, err = child.pipeline.Execute(ctx, tool.Call{Actor: root.ActorScope(), SessionID: root.SessionID, TurnID: "turn", CallID: "outer", ToolName: "flow_run", Registry: snapshot, RuntimeSnapshotID: identity, Arguments: json.RawMessage(`{"name":"test-flow","input":{},"idempotency_key":"nested"}`)})
			var constraint *ProfileConstraintError
			var recoverable *tool.ExecutionError
			if !errors.As(err, &constraint) || errors.As(err, &recoverable) {
				t.Fatalf("nested violation lost fatal classification: %v", err)
			}
			if writes.Load() != 0 {
				t.Fatal("nested Flow bypassed profile allowlist")
			}
			if scenario == "nested-budget" && reads.Load() != 1 {
				t.Fatalf("nested budget reads=%d", reads.Load())
			}
		})
	}
}

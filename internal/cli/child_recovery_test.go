package cli

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentmanager"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentprofile"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

type childRecoveryModel struct{ calls atomic.Int32 }

func (fake *childRecoveryModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	if fake.calls.Add(1) == 1 {
		return schema.AssistantMessage("", []schema.ToolCall{{ID: "write-once", Type: "function", Function: schema.FunctionCall{Name: "write_once", Arguments: `{}`}}}), nil
	}
	return schema.AssistantMessage("saved child result", nil), nil
}
func (fake *childRecoveryModel) Stream(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := fake.Generate(ctx, input, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func childRecoveryFactory(t *testing.T, cfg config.Config, fake *childRecoveryModel, writes *atomic.Int32) (*cliAppFixture, *agentruntime.Factory, orchestration.RootThread) {
	t.Helper()
	ctx := context.Background()
	database := openCLIApp(t, cfg)
	root, _, err := database.Orchestration().EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "root"})
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry()
	_, err = registry.Activate(tool.Binding{Descriptor: tool.Descriptor{Name: "write_once", Description: "count authorized writes", Revision: "v1", RiskClass: tool.RiskWrite, InputSchema: json.RawMessage(`{"type":"object"}`)}, Executor: tool.InvokeFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
		writes.Add(1)
		return json.RawMessage(`{}`), nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := policy.DefaultDeny(policy.Rule{ID: "allow-test", ToolPattern: "*", CapabilityPattern: "*", Effect: policy.EffectAllow})
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := tool.NewPipeline(tool.PipelineConfig{Registry: registry, Policy: engine, Events: database.Events()})
	if err != nil {
		t.Fatal(err)
	}
	factory, err := agentruntime.NewFactory(agentruntime.Config{ActorScope: root.ActorScope(), ModelProfile: "primary", MaxIterations: 5}, fake, registry, pipeline, database.Events(), database.Checkpoints())
	if err != nil {
		t.Fatal(err)
	}
	return database, factory, root
}

func createRecoveryChild(t *testing.T, database *cliAppFixture, root orchestration.RootThread) orchestration.AgentSnapshot {
	t.Helper()
	profile := agentprofile.Profile{Name: "worker", Revision: "worker-v1", Instruction: "Execute only the authorized task", ModelProfile: "primary", WorkspaceMode: agentprofile.WorkspaceSharedLease, ContextInheritance: agentprofile.InheritNone, Limits: agentprofile.Limits{MaxIterations: 5, MaxToolCalls: 5, MaxWallTime: time.Minute}}
	encoded, err := agentprofile.Encode(profile)
	if err != nil {
		t.Fatal(err)
	}
	child, err := database.Orchestration().CreateChild(context.Background(), orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: profile.Revision, ProfileSnapshot: encoded, InitialPrompt: "do one write"})
	if err != nil {
		t.Fatal(err)
	}
	runID, err := orchestration.NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	child, err = database.Orchestration().TransitionAgent(context.Background(), orchestration.AgentTransition{AgentID: child.ID, Expected: child.Version, Target: orchestration.AgentRunning, RunID: runID, Activity: "runner"})
	if err != nil {
		t.Fatal(err)
	}
	return child
}

func TestSupervisorRecoveryUsesOriginalRunWithoutRepeatingWrite(t *testing.T) {
	ctx := context.Background()
	cfg := cliATEConfig(t, config.Config{})
	fake, writes := &childRecoveryModel{}, &atomic.Int32{}
	database, factory, root := childRecoveryFactory(t, cfg, fake, writes)
	child := createRecoveryChild(t, database, root)
	result, err := newChildRunner(factory).Run(ctx, agentmanager.RunRequest{Agent: child, Prompt: child.InitialPrompt})
	if err != nil || result.Summary != "saved child result" || writes.Load() != 1 {
		t.Fatalf("initial=%#v writes=%d %v", result, writes.Load(), err)
	}
	// Simulate a process loss after turn.completed but before Supervisor.finish.
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, restored, _ := childRecoveryFactory(t, cfg, fake, writes)
	supervisor, err := agentmanager.NewSupervisor(agentmanager.Config{Agents: reopened.Orchestration(), Runner: newChildRunner(restored)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Close() })
	if err := supervisor.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	wait, err := supervisor.Wait(ctx, agentmanager.WaitCommand{AgentIDs: []orchestration.AgentID{child.ID}, Timeout: 5 * time.Second})
	if err != nil || !wait.Satisfied || len(wait.Agents) != 1 {
		t.Fatalf("wait=%#v %v", wait, err)
	}
	recovered := wait.Agents[0]
	if recovered.State != orchestration.AgentCompleted || recovered.CurrentRunID != child.CurrentRunID || recovered.Result == nil || recovered.Result.Summary != "saved child result" {
		t.Fatalf("wrong recovered child=%#v", recovered)
	}
	if fake.calls.Load() != 2 || writes.Load() != 1 {
		t.Fatalf("re-executed worker: calls=%d writes=%d", fake.calls.Load(), writes.Load())
	}
}

func TestChildRecoveryNeverFallsBackToFreshExecution(t *testing.T) {
	for _, scenario := range []string{"no-turn", "missing-checkpoint", "legacy-unbound-turn", "different-run"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			fake, writes := &childRecoveryModel{}, &atomic.Int32{}
			database, factory, root := childRecoveryFactory(t, cliATEConfig(t, config.Config{}), fake, writes)
			child := createRecoveryChild(t, database, root)
			if scenario != "no-turn" {
				runID := child.CurrentRunID
				if scenario == "legacy-unbound-turn" {
					runID = ""
				}
				if scenario == "different-run" {
					runID, _ = orchestration.NewRunID()
				}
				_, err := database.Events().Append(ctx, child.SessionID, session.AnySequence, session.PendingEvent{Type: session.EventTurnStarted, CorrelationID: "old-turn", Payload: session.MustJSONPayload(map[string]any{"run_id": runID})})
				if err != nil {
					t.Fatal(err)
				}
			}
			_, err := newChildRunner(factory).Run(ctx, agentmanager.RunRequest{Agent: child, Prompt: "must not restart", Recover: true})
			if err == nil || fake.calls.Load() != 0 || writes.Load() != 0 {
				t.Fatalf("unsafe fallback calls=%d writes=%d err=%v", fake.calls.Load(), writes.Load(), err)
			}
		})
	}
}

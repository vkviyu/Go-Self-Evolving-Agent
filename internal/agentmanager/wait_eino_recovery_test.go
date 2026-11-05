package agentmanager

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
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/sqlitestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenizer"
	appTool "github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

type einoWaitRecoveryModel struct {
	target    orchestration.AgentID
	calls     int
	summaries int
}

type einoWaitAdmissionGuard struct{ starts map[string]int }

func (*einoWaitAdmissionGuard) CheckCall(context.Context, appTool.Call, appTool.Descriptor) error {
	return nil
}

func (guard *einoWaitAdmissionGuard) BeforeStart(_ context.Context, call appTool.Call, _ appTool.Descriptor) (func(), error) {
	guard.starts[call.CallID]++
	if guard.starts[call.CallID] > 1 {
		return nil, errors.New("same invocation was admitted and charged twice")
	}
	return func() {}, nil
}

func (fake *einoWaitRecoveryModel) Generate(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	if len(input) > 0 && strings.HasPrefix(input[0].Content, "Summarize the conversation facts needed to continue") {
		fake.summaries++
		return schema.AssistantMessage(`{"version":1,"source":"semantic","task":"wait for the child","constraints":[],"completed":[],"changes":[],"verification":[],"open_items":[],"next_steps":[],"references":[]}`, nil), nil
	}
	fake.calls++
	if fake.calls == 1 {
		return schema.AssistantMessage("", []schema.ToolCall{{ID: "wait-eino-call", Type: "function", Function: schema.FunctionCall{Name: "agent_wait", Arguments: fmt.Sprintf(`{"agent_ids":[%q],"timeout_ms":60000}`, fake.target)}}}), nil
	}
	if len(input) == 0 || input[len(input)-1].Role != schema.Tool {
		return nil, errors.New("resumed Eino did not return the wait result to the model")
	}
	for _, message := range input {
		if strings.Contains(message.Content, "old findings old findings") {
			return nil, errors.New("checkpoint resume reintroduced the pre-compaction context")
		}
	}
	if fake.calls == 2 {
		var result WaitResult
		if err := json.Unmarshal([]byte(input[len(input)-1].Content), &result); err != nil || !result.Satisfied || result.TimedOut {
			return nil, fmt.Errorf("resumed wait result=%s err=%v", input[len(input)-1].Content, err)
		}
		return schema.AssistantMessage("", []schema.ToolCall{{ID: "wait-eino-read", Type: "function", Function: schema.FunctionCall{Name: "read", Arguments: `{}`}}}), nil
	}
	if input[len(input)-1].ToolCallID != "wait-eino-read" {
		return nil, errors.New("recovery lost confirmed work after the Eino checkpoint")
	}
	if fake.calls == 3 {
		return nil, errors.New("provider unavailable after confirmed checkpoint progress")
	}
	return schema.AssistantMessage("continued after the durable wait", nil), nil
}

func (fake *einoWaitRecoveryModel) Stream(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := fake.Generate(ctx, input, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func TestAgentWaitResumesThroughRealEinoCheckpointWithOriginalDeadline(t *testing.T) {
	ctx := context.Background()
	database, err := sqlitestore.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "wait-eino-root"})
	if err != nil {
		t.Fatal(err)
	}
	waiter, err := store.CreateChild(ctx, childSpec(root, "wait-eino-waiter", "wait for the child"))
	if err != nil {
		t.Fatal(err)
	}
	target, err := store.CreateChild(ctx, orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: waiter.ID, ProfileRevision: "worker-v1", IdempotencyKey: "wait-eino-target", InitialPrompt: "target"})
	if err != nil {
		t.Fatal(err)
	}
	runID, err := orchestration.NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	waiter, err = store.TransitionAgent(ctx, orchestration.AgentTransition{AgentID: waiter.ID, Expected: waiter.Version, Target: orchestration.AgentRunning, RunID: runID, Activity: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := NewSupervisor(Config{Agents: store, WaitPollInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Close() })
	registry := appTool.NewRegistry()
	reads := 0
	if _, err := registry.Activate(appTool.Binding{
		Descriptor: appTool.Descriptor{Name: "read", Description: "Read test state.", InputSchema: json.RawMessage(`{"type":"object"}`), Revision: "test-v1", RiskClass: appTool.RiskRead},
		Executor: appTool.InvokeFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
			reads++
			return json.RawMessage(`{"value":"stable"}`), nil
		}),
	}); err != nil {
		t.Fatal(err)
	}
	for _, binding := range Bindings(supervisor) {
		if binding.Descriptor.Name == "agent_wait" {
			if _, err := registry.Activate(binding); err != nil {
				t.Fatal(err)
			}
		}
	}
	engine, err := policy.DefaultDeny(policy.Rule{ID: "test-wait", ToolPattern: "*", Effect: policy.EffectAllow})
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := appTool.NewPipeline(appTool.PipelineConfig{Registry: registry, Policy: engine, Events: database.SessionEvents()})
	if err != nil {
		t.Fatal(err)
	}
	admission := &einoWaitAdmissionGuard{starts: make(map[string]int)}
	pipeline = pipeline.WithExecutionGuard(admission)
	fake := &einoWaitRecoveryModel{target: target.ID}
	factory, err := agentruntime.NewFactory(agentruntime.Config{
		Instruction: "Wait once, then report the result.", ContextWindowTokens: 8000,
		CompactionTriggerTokens: 1800, MaxOutputTokens: 256, Tokenizer: tokenizer.NewApproximate("test"),
	}, fake, registry, pipeline, database.SessionEvents(), database.Checkpoints())
	if err != nil {
		t.Fatal(err)
	}
	factory, err = factory.WithActorScope(waiter.ActorScope())
	if err != nil {
		t.Fatal(err)
	}
	turn, err := factory.NewTurn(ctx, waiter.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = turn.Run(ctx, []*schema.Message{
		schema.UserMessage(strings.Repeat("old question ", 600)),
		schema.AssistantMessage(strings.Repeat("old findings ", 600), nil),
		schema.UserMessage("wait for the child"),
	}, nil)
	suspended, ok := errors.AsType[*orchestration.WaitSuspendedError](err)
	if !ok || suspended.WaitSet.ID != "" || suspended.WaitSet.Deadline == nil {
		t.Fatalf("real Eino did not yield the original wait intent: %v", err)
	}
	records, err := factory.TurnRecords(ctx, waiter.SessionID)
	if err != nil || len(records) != 1 || !records[0].HasCheckpoint {
		t.Fatalf("Supervisor received suspension without its durable Eino checkpoint: records=%+v err=%v", records, err)
	}
	waitSet, err := supervisor.Suspend(ctx, suspended.WaitSpec())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.ResolveWaitSet(ctx, waitSet.ID, "target ready"); err != nil {
		t.Fatal(err)
	}
	queued, err := store.GetAgent(ctx, waiter.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionAgent(ctx, orchestration.AgentTransition{AgentID: waiter.ID, Expected: queued.Version, Target: orchestration.AgentRunning, RunID: runID, Activity: "fixture-resumed"}); err != nil {
		t.Fatal(err)
	}
	resumed, err := factory.ResumeTurn(ctx, waiter.SessionID, turn.ID())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.Resume(ctx, nil); err == nil || fake.calls != 3 || reads != 1 {
		t.Fatalf("expected a provider interruption after confirmed checkpoint progress: models=%d reads=%d err=%v", fake.calls, reads, err)
	}
	records, err = factory.TurnRecords(ctx, waiter.SessionID)
	if err != nil || len(records) != 1 || !records[0].HasCheckpoint {
		t.Fatalf("test requires an older surviving Eino checkpoint: records=%+v err=%v", records, err)
	}
	again, err := factory.ResumeTurn(ctx, waiter.SessionID, turn.ID())
	if err != nil {
		t.Fatal(err)
	}
	result, err := again.Resume(ctx, nil)
	if err != nil || result.Final == nil || result.Final.Content != "continued after the durable wait" || fake.calls != 4 || fake.summaries != 1 || reads != 1 {
		t.Fatalf("real Eino wait resume failed: result=%+v models=%d summaries=%d reads=%d err=%v", result, fake.calls, fake.summaries, reads, err)
	}
	stored, err := store.GetWaitSet(ctx, waitSet.ID)
	if err != nil || !stored.Deadline.Equal(*suspended.WaitSet.Deadline) {
		t.Fatalf("wait deadline changed across Eino resume: stored=%+v err=%v", stored, err)
	}
	if admission.starts["wait-eino-call"] != 1 || admission.starts["wait-eino-read"] != 1 {
		t.Fatalf("checkpoint replay charged tool quotas again: %+v", admission.starts)
	}
}

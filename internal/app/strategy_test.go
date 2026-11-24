package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/approval"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/strategy"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

type evolutionModel struct {
	calls   atomic.Int32
	offered atomic.Int32
}

func (m *evolutionModel) Generate(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.calls.Add(1)
	m.offered.Add(int32(len(model.GetCommonOptions(nil, options...).Tools)))
	answer := "BASE"
	for _, message := range input {
		if message.Role == schema.System && strings.Contains(message.Content, "candidate marker") {
			answer = "CANDIDATE"
		}
	}
	return schema.AssistantMessage(answer, nil), nil
}
func (m *evolutionModel) Stream(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := m.Generate(ctx, input, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func TestStrategyAgentApprovalIsolationAndReopen(t *testing.T) {
	ctx := context.Background()
	cfg := config.Config{DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir()}
	a, err := testOpen(t, ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	root, _, err := a.EnsureRootThread(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	fake := &evolutionModel{}
	factory, err := a.NewAgentFactory(fake, agentruntime.Config{ActorScope: root.ActorScope(), UsageLedger: a.Orchestration()})
	if err != nil {
		t.Fatal(err)
	}
	spec := strategy.Spec{Version: 1, Revision: "v1", InstructionSuffix: "candidate marker"}
	raw, _ := json.Marshal(spec)
	call := tool.Call{SessionID: "owner", TurnID: "t", CallID: "denied", ToolName: "strategy_propose", Actor: root.ActorScope(), Registry: a.Registry().Snapshot(), Arguments: raw}
	if _, err := a.Pipeline().WithApprover(approval.DenyApprover{}).Execute(ctx, call); err == nil {
		t.Fatal("proposal bypassed approval")
	}
	approved := a.Pipeline().WithApprover(approval.NewToolAllowlistApprover("strategy_propose", "strategy_shadow", "strategy_publish"))
	call.CallID = "propose"
	out, err := approved.Execute(ctx, call)
	if err != nil {
		t.Fatal(err)
	}
	var candidate struct {
		ID string `json:"candidate_id"`
	}
	if err := json.Unmarshal(out.Data, &candidate); err != nil || candidate.ID == "" {
		t.Fatalf("proposal=%s %v", out.Data, err)
	}
	stored, err := a.Strategies().Load(ctx, candidate.ID)
	if err != nil || stored.OwnerSession != "owner" {
		t.Fatal("untrusted owner", err)
	}
	suite := strategy.Suite{Name: "host-only", Fixtures: []strategy.Fixture{{Name: "safety", Prompt: "literal response", Expected: "CANDIDATE", Safety: true}}}
	suiteID, err := a.Strategies().SaveSuite(ctx, suite, true)
	if err != nil {
		t.Fatal(err)
	}
	call.ToolName, call.CallID = "strategy_shadow", "shadow"
	call.Arguments, _ = json.Marshal(map[string]any{"candidate_id": candidate.ID, "suite_id": suiteID})
	out, err = approved.Execute(ctx, call)
	if err != nil {
		t.Fatal(err)
	}
	var evaluation struct {
		ID     string          `json:"report_id"`
		Report strategy.Report `json:"report"`
	}
	if err := json.Unmarshal(out.Data, &evaluation); err != nil || !evaluation.Report.Eligible {
		t.Fatalf("shadow=%s %v", out.Data, err)
	}
	if fake.calls.Load() != 2 || fake.offered.Load() != 0 {
		t.Fatal("shadow had business tools or extra calls")
	}
	if current, _ := a.Strategies().Active(ctx, factory.BaselineConfigID()); current != nil {
		t.Fatal("test activated candidate")
	}
	foreign, _, err := a.EnsureRootThread(ctx, "foreign")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.NewAgentFactory(fake, agentruntime.Config{ActorScope: foreign.ActorScope()}); err != nil {
		t.Fatal(err)
	}
	call.ToolName, call.CallID, call.SessionID, call.Actor = "strategy_inspect", "foreign", "foreign", foreign.ActorScope()
	call.Arguments, _ = json.Marshal(map[string]any{"report_id": evaluation.ID})
	if _, err := approved.Execute(ctx, call); err == nil {
		t.Fatal("foreign root read strategy")
	}
	call.Actor = root.ActorScope()
	call.Actor.ParentAgentID = root.AgentID
	call.SessionID = "owner"
	call.CallID = "child"
	if _, err := approved.Execute(ctx, call); err == nil {
		t.Fatal("child bypassed root boundary")
	}
	call.ToolName, call.CallID, call.Actor = "strategy_publish", "publish-denied", root.ActorScope()
	if _, err := a.Pipeline().WithApprover(approval.DenyApprover{}).Execute(ctx, call); err == nil {
		t.Fatal("publication bypassed approval")
	}
	call.CallID = "publish"
	if _, err := approved.Execute(ctx, call); err != nil {
		t.Fatal(err)
	}
	turn, err := factory.NewTurn(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	result, err := turn.Run(ctx, []*schema.Message{schema.UserMessage("hello")}, nil)
	if err != nil || result.Final.Content != "CANDIDATE" {
		t.Fatalf("result=%+v %v", result, err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := testOpen(t, ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	newFactory, err := reopened.NewAgentFactory(fake, agentruntime.Config{ActorScope: root.ActorScope(), UsageLedger: reopened.Orchestration()})
	if err != nil {
		t.Fatal(err)
	}
	if newFactory.BaselineConfigID() != factory.BaselineConfigID() {
		t.Fatal("baseline identity not stable across restart")
	}
	if active, err := reopened.Strategies().Active(ctx, newFactory.BaselineConfigID()); err != nil || active == nil || active.ID() != candidate.ID {
		t.Fatalf("active=%+v %v", active, err)
	}
	if _, exists, err := newFactory.CompletedTurn(ctx, "owner", turn.ID()); err != nil || !exists {
		t.Fatal("historical strategy turn not restored", err)
	}
	changed, err := reopened.NewAgentFactory(fake, agentruntime.Config{ActorScope: root.ActorScope(), StrategyScope: "different gateway"})
	if err != nil {
		t.Fatal(err)
	}
	if changed.BaselineConfigID() == newFactory.BaselineConfigID() {
		t.Fatal("gateway drift reused shadow evidence")
	}
}

func TestEvolutionWorkersCancelAndJoinBeforeClose(t *testing.T) {
	a, err := testOpen(t, context.Background(), config.Config{DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	worker, finish, err := a.beginEvolution(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	go func() { <-worker.Done(); finish(); close(finished) }()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("close returned before worker stopped")
	}
	if _, _, err := a.ShadowStrategy(context.Background(), nil, "", ""); err == nil {
		t.Fatal("shadow started after close")
	}
}

func TestStrategyEvidenceErrorsAreFatalToTheEinoAdapter(t *testing.T) {
	binding := tool.Binding{Descriptor: tool.Descriptor{Name: "evidence_fault", Revision: "v1", Description: "failure injection", RiskClass: tool.RiskExecute, InputSchema: []byte(`{"type":"object"}`)}, Executor: tool.InvokeFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) { return nil, strategy.ErrEvidence })}
	a, err := testOpenWithOptions(t, context.Background(), config.Config{DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir()}, OpenOptions{ToolBindings: []tool.Binding{binding}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	_, err = a.Pipeline().WithApprover(approval.NewToolAllowlistApprover("evidence_fault")).Execute(context.Background(), tool.Call{SessionID: "evidence", TurnID: "turn", CallID: "call", ToolName: "evidence_fault", Arguments: []byte(`{}`)})
	var recoverable *tool.ExecutionError
	if !errors.Is(err, strategy.ErrEvidence) || errors.As(err, &recoverable) {
		t.Fatalf("uncertainty became repairable: %v", err)
	}
}

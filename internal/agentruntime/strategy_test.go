package agentruntime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
	"github.com/vkviyu/go-self-evolving-agent/internal/strategy"
)

type strategyMemory struct {
	active    *strategy.Spec
	specs     map[string]strategy.Spec
	fallbacks int
}

func (s *strategyMemory) Active(context.Context, string) (*strategy.Spec, error) {
	return s.active, nil
}
func (s *strategyMemory) Load(_ context.Context, id string) (strategy.Spec, error) {
	spec, ok := s.specs[id]
	if !ok {
		return spec, errors.New("missing spec")
	}
	return spec, nil
}
func (s *strategyMemory) Fallback(_ context.Context, base, id, reason string) error {
	if s.active != nil && s.active.ID() == id {
		s.active = nil
		s.fallbacks++
	}
	return nil
}

type strategyModel struct {
	fail        bool
	toolAttempt bool
	requests    int
	tools       int
}

func (m *strategyModel) Generate(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.requests++
	common := model.GetCommonOptions(nil, options...)
	m.tools += len(common.Tools)
	if m.fail {
		return nil, errors.New("synthetic model failure")
	}
	if m.toolAttempt {
		return schema.AssistantMessage("", []schema.ToolCall{{ID: "bad", Type: "function", Function: schema.FunctionCall{Name: "workspace_write", Arguments: `{"path":"forbidden"}`}}}), nil
	}
	answer := "BASE"
	for _, message := range input {
		if message.Role == schema.System && strings.Contains(message.Content, "candidate marker") {
			answer = "CANDIDATE"
		}
	}
	return schema.AssistantMessage(answer, nil), nil
}
func (m *strategyModel) Stream(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := m.Generate(ctx, input, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func TestStrategyPinsTurnRestoresHistoricalSpecAndFallsBackWithoutReplay(t *testing.T) {
	ctx := context.Background()
	events := memorystore.New()
	fake := &strategyModel{}
	f := profileFactory(t, events, fake)
	spec := strategy.Spec{Version: 1, Revision: "v1", InstructionSuffix: "candidate marker", MaxIterations: 100, MaxOutputTokens: 32768}
	store := &strategyMemory{specs: map[string]strategy.Spec{spec.ID(): spec}}
	f.config.Strategies = store
	old, err := f.NewTurn(ctx, "session")
	if err != nil {
		t.Fatal(err)
	}
	store.active = &spec
	selected, err := f.withActiveStrategy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if selected.config.MaxIterations != f.config.MaxIterations || selected.config.MaxOutputTokens != f.config.MaxOutputTokens {
		t.Fatal("strategy increased host caps")
	}
	result, err := old.Run(ctx, []*schema.Message{schema.UserMessage("hello")}, nil)
	if err != nil || result.Final.Content != "BASE" {
		t.Fatalf("old turn mutated: %+v %v", result, err)
	}
	turn, err := f.NewTurn(ctx, "session")
	if err != nil {
		t.Fatal(err)
	}
	result, err = turn.Run(ctx, []*schema.Message{schema.UserMessage("hello")}, nil)
	if err != nil || result.Final.Content != "CANDIDATE" {
		t.Fatalf("new turn=%+v %v", result, err)
	}
	store.active = nil
	recovered, err := f.withTurnStrategy(ctx, "session", turn.ID())
	if err != nil || recovered.strategyID != spec.ID() {
		t.Fatal("lost pinned strategy", err)
	}
	if _, _, err := f.CompletedTurn(ctx, "session", turn.ID()); err != nil {
		t.Fatal("completed strategy turn not restorable", err)
	}
	if base, err := f.withTurnStrategy(ctx, "session", old.ID()); err != nil || base.strategyID != "" {
		t.Fatal("old base turn inherited active strategy", err)
	}
	store.active = &spec
	fake.fail = true
	failing, err := f.NewTurn(ctx, "session")
	if err != nil {
		t.Fatal(err)
	}
	requests := fake.requests
	if _, err := failing.Run(ctx, []*schema.Message{schema.UserMessage("fail")}, nil); err == nil {
		t.Fatal("expected failure")
	}
	if store.active != nil || store.fallbacks != 1 || fake.requests != requests+1 {
		t.Fatal("failure retried work or did not fall back")
	}
	if next, err := f.NewTurn(ctx, "session"); err != nil || next.strategyID != "" {
		t.Fatal("future turn did not use base", err)
	}
	if recovered, err := f.withTurnStrategy(ctx, "session", failing.ID()); err != nil || recovered.strategyID != spec.ID() {
		t.Fatal("fallback rewrote old turn", err)
	}
}

func TestStrategyShadowIsToolFreeAndMeasuresTheSameTrustedSuite(t *testing.T) {
	ctx := context.Background()
	fake := &strategyModel{}
	f := profileFactory(t, memorystore.New(), fake)
	spec := strategy.Spec{Version: 1, Revision: "v1", InstructionSuffix: "candidate marker"}
	suite := strategy.Suite{Name: "fixed", Fixtures: []strategy.Fixture{{Name: "safety", Prompt: "literal response", Expected: "CANDIDATE", Safety: true}}}
	report, err := f.CompareStrategy(ctx, spec, suite)
	if err != nil || !report.Eligible {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if fake.requests != 2 || fake.tools != 0 || report.Baseline[0].Passed || !report.Candidate[0].Passed || report.Candidate[0].ModelCalls != 1 {
		t.Fatalf("not isolated: %+v fake=%+v", report, fake)
	}
	fake.toolAttempt = true
	report, err = f.CompareStrategy(ctx, spec, suite)
	if err != nil || report.Eligible {
		t.Fatalf("tool attempt passed gate: %+v %v", report, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	before := fake.requests
	report, err = f.CompareStrategy(canceled, spec, suite)
	if err != nil || report.Eligible || fake.requests != before {
		t.Fatalf("canceled shadow dispatched: %+v %v", report, err)
	}
}

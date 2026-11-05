package app

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

// usageModel answers with fixed provider-reported usage.
type usageModel struct{}

func (usageModel) Generate(ctx context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	message := schema.AssistantMessage("done", nil)
	message.ResponseMeta = &schema.ResponseMeta{Usage: &schema.TokenUsage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500}}
	return message, nil
}

func (m usageModel) Stream(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := m.Generate(ctx, input, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func TestUsageConfigPricesTurnsAndBoundsTheRootTeam(t *testing.T) {
	ctx := context.Background()
	cfg := config.Config{DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir(), Usage: config.UsageConfig{
		Prices: map[string]tokenusage.Price{"fake/priced": {InputMicrosPerMillion: 2_000_000, OutputMicrosPerMillion: 4_000_000}},
		Budget: tokenusage.BudgetLimits{MaxCostMicros: 1_000_000_000},
	}}
	a, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	root, _, err := a.EnsureRootThread(ctx, "priced")
	if err != nil {
		t.Fatal(err)
	}
	budget, err := a.Orchestration().Budget(ctx, root.TeamID)
	if err != nil || budget.Limits != cfg.Usage.Budget {
		t.Fatalf("budget = %+v, err=%v; want limits %+v", budget, err, cfg.Usage.Budget)
	}
	factory, err := a.NewAgentFactory(usageModel{}, agentruntime.Config{
		ActorScope: root.ActorScope(), UsageLedger: a.Orchestration(), Provider: "fake", Model: "priced", ModelProfile: "priced", MaxOutputTokens: 256,
	})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := factory.NewTurn(ctx, "priced")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := turn.Run(ctx, []*schema.Message{schema.UserMessage("hello")}, nil); err != nil {
		t.Fatal(err)
	}
	report, err := a.Orchestration().UsageReport(ctx, tokenusage.UsageQuery{TeamID: root.TeamID})
	if err != nil {
		t.Fatal(err)
	}
	// 1000 input tokens at 2 and 500 output tokens at 4 micro-units each.
	if report.Aggregate.TotalTokens != 1500 || report.Aggregate.CostMicros != 4000 {
		t.Fatalf("usage = %+v, want 1500 tokens costing 4000 micros", report.Aggregate)
	}
	// The next reservation is priced above zero, so a limit just above the
	// recorded cost rejects it; unpriced usage would still fit.
	if err := a.Orchestration().ConfigureBudget(ctx, root.TeamID, tokenusage.BudgetLimits{MaxCostMicros: 4001}); err != nil {
		t.Fatal(err)
	}
	next, err := factory.NewTurn(ctx, "priced")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := next.Run(ctx, []*schema.Message{schema.UserMessage("again")}, nil); err == nil || !strings.Contains(err.Error(), tokenusage.ErrBudgetExceeded.Error()) {
		t.Fatalf("turn over budget error = %v", err)
	}
}

func TestRootThreadClearsABudgetRemovedFromTheConfig(t *testing.T) {
	ctx := context.Background()
	cfg := config.Config{DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir(), Usage: config.UsageConfig{Budget: tokenusage.BudgetLimits{MaxCostMicros: 10}}}
	for _, limits := range []tokenusage.BudgetLimits{cfg.Usage.Budget, {}} {
		cfg.Usage.Budget = limits
		a, err := Open(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		root, _, err := a.EnsureRootThread(ctx, "budgeted")
		if err != nil {
			t.Fatal(err)
		}
		budget, err := a.Orchestration().Budget(ctx, root.TeamID)
		if closeErr := a.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if err != nil || budget.Limits != limits {
			t.Fatalf("budget = %+v, err=%v; want %+v", budget.Limits, err, limits)
		}
	}
}

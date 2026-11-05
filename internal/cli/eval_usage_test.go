package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/app"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/evaluation"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimeowner"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

func TestEvaluationUsageHonorsRuntimeOwnershipAndMapping(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg := config.Config{DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir()}
	a, err := app.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	root, _, err := a.EnsureRootThread(ctx, "usage-fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Orchestration().RecordUsage(ctx, tokenusage.Record{
		TeamID: root.TeamID, AgentID: root.AgentID, RequestID: "fixture",
		Usage: tokenusage.Snapshot{Input: 10, Cached: 3, Output: 8, Reasoning: 4, Total: 18}, CostMicros: 9,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := readEvaluationUsage(ctx, cfg.DataDirectory, string(root.SessionID)); !errors.Is(err, runtimeowner.ErrOwned) {
		t.Fatalf("evaluator bypassed live runtime ownership: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	want := evaluation.Usage{Input: 10, Cached: 3, Output: 8, Reasoning: 4, Total: 18, Records: 1, CostMicros: 9}
	got, err := readEvaluationUsage(ctx, cfg.DataDirectory, string(root.SessionID))
	if err != nil || got != want {
		t.Fatalf("usage = %+v, %v; want %+v", got, err, want)
	}
}

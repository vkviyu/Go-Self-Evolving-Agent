package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/vkviyu/go-self-evolving-agent/internal/app"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/evaluation"
	"github.com/vkviyu/go-self-evolving-agent/internal/modelgateway"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimeowner"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

func TestEvaluationUsageHonorsRuntimeOwnershipAndMapping(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg := cliATEConfig(t, config.Config{DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir()})
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
	if _, err := readEvaluationUsage(ctx, cfg, string(root.SessionID)); !errors.Is(err, runtimeowner.ErrOwned) {
		t.Fatalf("evaluator bypassed live runtime ownership: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	want := evaluation.Usage{Input: 10, Cached: 3, Output: 8, Reasoning: 4, Total: 18, Records: 1, CostMicros: 9}
	// Prove the fixture survives reopening through the native authority before
	// checking whether the CLI preserves the configuration needed to reach it.
	report, err := app.ReadSessionUsage(ctx, cfg, root.SessionID)
	if err != nil {
		t.Fatalf("read native fixture with complete configuration: %v", err)
	}
	aggregate := report.Aggregate
	native := evaluation.Usage{
		Input: aggregate.InputTokens, Cached: aggregate.CachedTokens,
		Output: aggregate.OutputTokens, Reasoning: aggregate.ReasoningTokens,
		Total: aggregate.TotalTokens, Records: aggregate.Records, CostMicros: aggregate.CostMicros,
	}
	if native != want {
		t.Fatalf("native fixture usage = %+v, want %+v", native, want)
	}
	got, err := readEvaluationUsage(ctx, cfg, string(root.SessionID))
	if err != nil || got != want {
		t.Fatalf("usage = %+v, %v; want %+v", got, err, want)
	}
}

func TestEvaluationAgentReadsUsageFromOriginalProfileAuthority(t *testing.T) {
	cliATEEnvironment(t)
	profile := nativeCLIProfile(t)
	args := []string{"--profile", profile, "--model", "test-model"}
	cfg, err := loadChatConfig(args)
	if err != nil {
		t.Fatal(err)
	}
	workspace, runDirectory := t.TempDir(), t.TempDir()
	otherAuthority := filepath.Join(t.TempDir(), "unused-ate")
	deps := dependencies{newModel: func(context.Context, modelgateway.Config) (model.ToolCallingChatModel, error) {
		// Simulate editing the profile while evaluation is running. Re-reading
		// defaults or the profile for usage would select the wrong authority.
		path := filepath.Join(cfg.HomeDirectory, profile+".config.toml")
		if err := os.WriteFile(path, []byte(fmt.Sprintf("[ate]\nenabled = true\ndirectory = %q\n", otherAuthority)), 0600); err != nil {
			t.Fatal(err)
		}
		return &reasoningChatModel{}, nil
	}}
	outcome, err := evalAgentFunc(deps, args, "usage-run")(t.Context(), evaluation.AgentRequest{
		TaskID: "usage-task", ProblemStatement: "Reply with an answer.",
	}, workspace, runDirectory)
	if err != nil || outcome.Status != "completed" || outcome.Error != "" {
		t.Fatalf("evaluation outcome=%+v err=%v", outcome, err)
	}
	cfg.DataDirectory = filepath.Join(runDirectory, "data")
	cfg.WorkspaceDirectory = workspace
	want, err := readEvaluationUsage(t.Context(), cfg, outcome.SessionID)
	if err != nil || want.Records == 0 || want.Total == 0 || outcome.Usage != want {
		t.Fatalf("evaluation usage=%+v native usage=%+v err=%v", outcome.Usage, want, err)
	}
	if _, err := os.Stat(otherAuthority); !os.IsNotExist(err) {
		t.Fatalf("usage opened a replacement authority: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.HomeDirectory, "temporal")); !os.IsNotExist(err) {
		t.Fatalf("usage opened the default authority instead of the profile: %v", err)
	}
}

package app

import (
	"context"
	"errors"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimeowner"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

func TestReadSessionUsageOwnsDatabaseAndIncludesOnlyItsTeam(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg := testConfig(t, config.Config{DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir()})
	a, err := testOpen(t, ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	root, _, err := a.EnsureRootThread(ctx, "usage-root")
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := a.EnsureRootThread(ctx, "other-root")
	if err != nil {
		t.Fatal(err)
	}
	child, err := a.Orchestration().CreateChild(ctx, orchestration.SpawnSpec{
		TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker-v1",
		IdempotencyKey: "usage-child", InitialPrompt: "local usage fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range []tokenusage.Record{
		{TeamID: root.TeamID, AgentID: root.AgentID, RequestID: "root", Usage: tokenusage.Snapshot{Input: 10, Cached: 2, Output: 5, Reasoning: 3, Total: 15}, CostMicros: 7},
		{TeamID: root.TeamID, AgentID: child.ID, RequestID: "child", Usage: tokenusage.Snapshot{Input: 20, Cached: 4, Output: 10, Reasoning: 6, Total: 30}, CostMicros: 14},
		{TeamID: other.TeamID, AgentID: other.AgentID, RequestID: "other", Usage: tokenusage.Snapshot{Total: 999}},
	} {
		if err := a.Orchestration().RecordUsage(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ReadSessionUsage(ctx, cfg, root.SessionID); !errors.Is(err, runtimeowner.ErrOwned) {
		t.Fatalf("live runtime ownership bypassed: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	want := tokenusage.UsageAggregate{Records: 2, InputTokens: 30, CachedTokens: 6, OutputTokens: 15, ReasoningTokens: 9, TotalTokens: 45, CostMicros: 21}
	for range 2 {
		report, err := ReadSessionUsage(ctx, cfg, root.SessionID)
		if err != nil || report.Query.TeamID != root.TeamID || report.Aggregate != want {
			t.Fatalf("report=%+v, error=%v; want %+v", report, err, want)
		}
	}
}

func TestOfflineUsageFailureReleasesOwnership(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg := testConfig(t, config.Config{DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir()})
	if _, err := ReadSessionUsage(ctx, cfg, session.ID("missing")); !errors.Is(err, orchestration.ErrNotFound) {
		t.Fatalf("missing root error = %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := ReadSessionUsage(canceled, cfg, "missing"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read error = %v", err)
	}
	a, err := testOpen(t, ctx, cfg)
	if err != nil {
		t.Fatalf("failed offline reads retained ownership: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
}

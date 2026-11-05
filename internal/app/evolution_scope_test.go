package app

import (
	"context"
	"strings"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func TestStrategyCallerRequiresMatchingDurableRootAndFactory(t *testing.T) {
	ctx := context.Background()
	a, err := Open(ctx, config.Config{DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Error(err)
		}
	})
	root, _, err := a.EnsureRootThread(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	foreign, _, err := a.EnsureRootThread(ctx, "foreign")
	if err != nil {
		t.Fatal(err)
	}
	child := root.ActorScope()
	child.ParentAgentID = root.AgentID
	for _, tc := range []struct {
		name    string
		session session.ID
		actor   orchestration.ActorScope
		want    string
	}{
		{"no actor", "owner", orchestration.ActorScope{}, "strategy evolution requires a configured root Agent"},
		{"child actor", "owner", child, "strategy evolution requires a configured root Agent"},
		{"wrong root", "owner", foreign.ActorScope(), "strategy caller does not match the session's root Agent"},
		{"unknown session", "missing", root.ActorScope(), "strategy caller does not match the session's root Agent"},
		{"no factory", "owner", root.ActorScope(), "root model factory is unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := a.Pipeline().Execute(ctx, tool.Call{SessionID: tc.session, TurnID: "scope", CallID: tc.name,
				Actor: tc.actor, ToolName: "strategy_inspect", Arguments: []byte(`{"candidate_id":"missing"}`)})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v want=%s", err, tc.want)
			}
		})
	}
}

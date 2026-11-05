package app

import (
	"context"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentmanager"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func TestBuildPolicyRequiresApprovalForInvocationTools(t *testing.T) {
	engine, err := buildPolicy([]tool.Binding{{
		Descriptor: tool.Descriptor{Name: "search_calendar_events"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := engine.Evaluate(context.Background(), policy.Request{
		ToolName: "search_calendar_events", Capability: "tool.invoke",
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Effect != policy.EffectRequireApproval {
		t.Fatalf("effect = %q", decision.Effect)
	}
	unknown, err := engine.Evaluate(context.Background(), policy.Request{
		ToolName: "undeclared_external_tool", Capability: "tool.invoke",
	})
	if err != nil {
		t.Fatal(err)
	}
	if unknown.Effect != policy.EffectDeny {
		t.Fatalf("unknown tool effect = %q", unknown.Effect)
	}
}

// Every first-party tool the production assembly registers must be named by
// an explicit rule; falling through to the default denies it silently.
func TestBuildPolicyNamesEveryRegisteredTool(t *testing.T) {
	application, err := Open(context.Background(), config.Config{
		HomeDirectory: t.TempDir(), DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir(), CoordinationDirectory: t.TempDir(),
		Goal: agentgoal.Config{Enabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	if _, err := application.NewSupervisor(agentmanager.Config{}); err != nil {
		t.Fatal(err)
	}
	engine, err := buildPolicy(nil)
	if err != nil {
		t.Fatal(err)
	}
	descriptors := application.Registry().Snapshot().Descriptors()
	names := make(map[string]bool, len(descriptors))
	for _, descriptor := range descriptors {
		names[descriptor.Name] = true
		decision, err := engine.Evaluate(context.Background(), policy.Request{ToolName: descriptor.Name, Capability: "tool.invoke"})
		if err != nil {
			t.Fatal(err)
		}
		if decision.RuleID == "default" {
			t.Errorf("registered tool %s has no policy rule", descriptor.Name)
		}
	}
	for _, required := range []string{"task_verify", "agent_spawn", "merge_run", "goal_start"} {
		if !names[required] {
			t.Errorf("tool %s is not registered", required)
		}
	}
}

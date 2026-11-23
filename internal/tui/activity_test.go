package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentmanager"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

type fakeActivityProvider struct{}

func (fakeActivityProvider) LoadTeamSnapshot(context.Context, orchestration.TeamID) (agentmanager.TeamSnapshot, error) {
	return agentmanager.TeamSnapshot{
		Agents: []orchestration.AgentSnapshot{{ID: "agent-root", State: orchestration.AgentRunning, Activity: "model"}},
		Tasks:  []orchestration.TaskSnapshot{{ID: "task-build", State: orchestration.TaskReady, Title: "build project"}},
		Views:  []workspacecoord.View{{ID: "view-main", Mode: workspacecoord.ViewSharedLease, State: workspacecoord.ViewActive}},
		Merges: []workspacecoord.MergeRequest{{ID: "merge-1", State: workspacecoord.MergeConflicted, Reason: "patch conflict"}},
	}, nil
}

func TestActivityPanelLoadsAndRendersControlPlaneSnapshot(t *testing.T) {
	model := newTestModel(t)
	model.teamID = "team-test"
	model.activityProvider = fakeActivityProvider{}
	command := model.toggleActivity()
	if command == nil {
		t.Fatal("toggle activity did not start refresh")
	}
	message := command()
	refresh, ok := message.(activityRefreshMsg)
	if !ok {
		t.Fatalf("activity command returned %T, want activityRefreshMsg", message)
	}
	model.activity = refresh.snapshot
	model.activityLoading = false
	model.resizeInput()
	panel := ansi.Strip(model.renderActivityPanel(model.width - 2))
	for _, expected := range []string{"merges 1 pending", "agent-root", "task-build", "view-main", "merge-1", "conflicted"} {
		if !strings.Contains(panel, expected) {
			t.Fatalf("activity panel missing %q: %q", expected, panel)
		}
	}
	if model.viewport.Height >= 24-1-model.input.Height() {
		t.Fatalf("activity panel did not reserve viewport height: %d", model.viewport.Height)
	}
	if command := model.toggleActivity(); command != nil {
		t.Fatal("hiding activity unexpectedly started refresh")
	}
}

func TestActivityPanelTruncatesRowsWithoutOverflow(t *testing.T) {
	model := newTestModel(t)
	model.showActivity = true
	model.activity.Agents = make([]orchestration.AgentSnapshot, 20)
	for index := range model.activity.Agents {
		model.activity.Agents[index] = orchestration.AgentSnapshot{ID: orchestration.AgentID("agent-long-identifier-" + string(rune('a'+index))), State: orchestration.AgentRunning}
	}
	model.width = 48
	panel := model.renderActivityPanel(model.width - 2)
	if strings.Count(panel, "more") != 1 {
		t.Fatalf("activity overflow marker count = %d, panel=%q", strings.Count(panel, "more"), ansi.Strip(panel))
	}
	for line := range strings.SplitSeq(panel, "\n") {
		if ansi.StringWidth(line) > model.width-2 {
			t.Fatalf("activity line width=%d exceeds %d: %q", ansi.StringWidth(line), model.width-2, ansi.Strip(line))
		}
	}
}

func TestMergeCommandAddsResolutionSummaryToTranscript(t *testing.T) {
	model := newTestModel(t)
	model.activity.Merges = []workspacecoord.MergeRequest{{
		ID: "merge-conflict", State: workspacecoord.MergeConflicted,
		ResolutionTaskID: "task-resolution", Reason: "patch did not apply cleanly",
	}}
	if _, handled := model.handleCommand("/merge"); !handled {
		t.Fatal("merge command was not handled")
	}
	plain := ansi.Strip(model.transcriptContent)
	for _, expected := range []string{"merge-conflict", "conflicted", "task-resolution", "patch did not apply cleanly"} {
		if !strings.Contains(plain, expected) {
			t.Fatalf("merge summary missing %q: %q", expected, plain)
		}
	}
}

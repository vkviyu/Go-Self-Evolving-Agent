package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

type fakeAgentControl struct {
	interrupted []orchestration.AgentID
	reviewed    []orchestration.TeamID
	discarded   []orchestration.MergeID
}

func (control *fakeAgentControl) Interrupt(_ context.Context, agentID orchestration.AgentID) error {
	control.interrupted = append(control.interrupted, agentID)
	return nil
}

func (control *fakeAgentControl) ReviewMerges(teamID orchestration.TeamID) error {
	control.reviewed = append(control.reviewed, teamID)
	return nil
}

func (control *fakeAgentControl) DiscardMerge(_ context.Context, mergeID orchestration.MergeID, _ string) (workspacecoord.MergeRequest, error) {
	control.discarded = append(control.discarded, mergeID)
	return workspacecoord.MergeRequest{ID: mergeID, State: workspacecoord.MergeRejected}, nil
}

type fakeTranscriptReader struct {
	sessions map[session.ID][]*schema.Message
}

func (reader fakeTranscriptReader) LoadTranscript(_ context.Context, sessionID session.ID) ([]*schema.Message, error) {
	return reader.sessions[sessionID], nil
}

// newPanelModel shows the /agents panel over a root, a running child, a
// queued child and a pending merge.
func newPanelModel(t *testing.T) (*Model, *fakeAgentControl) {
	t.Helper()
	model := newTestModel(t)
	control := &fakeAgentControl{}
	model.teamID = "team-test"
	model.agentControl = control
	model.transcripts = fakeTranscriptReader{sessions: map[session.ID][]*schema.Message{
		"session-child": {schema.UserMessage("inspect the parser"), schema.AssistantMessage("parser inspected", nil)},
	}}
	model.activity.Agents = []orchestration.AgentSnapshot{
		{ID: "agent-root", State: orchestration.AgentRunning},
		{ID: "agent-child", ParentAgentID: "agent-root", SessionID: "session-child", State: orchestration.AgentRunning, Activity: "tool read_file"},
		{ID: "agent-queued", ParentAgentID: "agent-root", State: orchestration.AgentQueued},
	}
	model.activity.Merges = []workspacecoord.MergeRequest{{ID: "merge-1", State: workspacecoord.MergePending}}
	model.activity.Usage = map[orchestration.AgentID]tokenusage.UsageAggregate{"agent-child": {TotalTokens: 12500}}
	model.handleCommand("/agents")
	return model, control
}

func pressRune(model *Model, value rune) tea.Cmd {
	command, _ := model.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{value}})
	return command
}

// runAction executes an action command and applies its result message.
func runAction(t *testing.T, model *Model, command tea.Cmd) {
	t.Helper()
	if command == nil {
		t.Fatal("panel action returned no command")
	}
	model.Update(command())
}

func TestAgentsPanelShowsLiveActivityTokensAndSummary(t *testing.T) {
	model, _ := newPanelModel(t)
	panel := ansi.Strip(model.renderActivityPanel(model.width - 2))
	for _, expected := range []string{"Child agents 2 · 1 running · 1 queued", "merges 1 pending", "Ctrl+G", "agent-root (root)  idle", "tool read_file", "12k tok", "› agent agent-root"} {
		if !strings.Contains(panel, expected) {
			t.Fatalf("panel missing %q:\n%s", expected, panel)
		}
	}
	model.toggleActivity()
	panel = ansi.Strip(model.renderActivityPanel(model.width - 2))
	if !strings.Contains(panel, "● main") || !strings.Contains(panel, "↳ agent-child") || strings.Contains(panel, "merge merge-1") {
		t.Fatalf("collapsed panel lacks the quick Agent switcher: %q", panel)
	}
}

func TestMouseSwitchesRunningAgentsWithoutChangingRootInput(t *testing.T) {
	model, _ := newPanelModel(t)
	model.toggleActivity()
	model.activity.Agents[2].SessionID = "session-other"
	model.transcripts = fakeTranscriptReader{sessions: map[session.ID][]*schema.Message{
		"session-child": {schema.AssistantMessage("first child", nil)},
		"session-other": {schema.AssistantMessage("second child", nil)},
	}}
	model.blocks = []transcriptBlock{{kind: blockUser, content: "root question"}}
	model.busy = true
	model.input.SetValue("保留输入草稿")
	model.resize(84, 24)
	click := func(panelRow int) tea.Cmd {
		_, command := model.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 2, Y: model.headerHeight() + 1 + panelRow})
		return command
	}
	runAction(t, model, click(2))
	if model.viewer == nil || model.viewer.agentID != "agent-child" || !strings.Contains(ansi.Strip(model.transcriptContent), "first child") {
		t.Fatalf("first Agent not opened: %q", model.transcriptContent)
	}
	runAction(t, model, click(3))
	if model.viewer == nil || model.viewer.agentID != "agent-queued" || !strings.Contains(ansi.Strip(model.transcriptContent), "second child") {
		t.Fatalf("second Agent not opened: %q", model.transcriptContent)
	}
	click(1)
	if model.viewer != nil || !model.busy || model.input.Value() != "保留输入草稿" || !strings.Contains(ansi.Strip(model.transcriptContent), "root question") {
		t.Fatal("clicking main changed the active root turn, input, or transcript")
	}
}

func TestMouseOpensAgentFromScrolledFullPanel(t *testing.T) {
	model, _ := newPanelModel(t)
	for index := range 10 {
		agent := model.activity.Agents[1]
		agent.ID = orchestration.AgentID("agent-extra-" + string(rune('a'+index)))
		model.activity.Agents = append(model.activity.Agents, agent)
	}
	model.resize(90, 16)
	const y = 4
	for range 11 {
		model.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown, X: 2, Y: y})
	}
	selected, ok := model.selectedAgent()
	if !ok || selected.ID != "agent-extra-i" {
		t.Fatalf("wheel did not scroll the Agents list: %v", selected.ID)
	}
	for index, row := range model.activityPanelRows(model.width - 2) {
		if row.selectable && row.index == model.panelSelected {
			_, command := model.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 2, Y: model.headerHeight() + 1 + index})
			runAction(t, model, command)
			if model.viewer == nil || model.viewer.agentID != selected.ID {
				t.Fatal("mouse hit the wrong Agent after scrolling")
			}
			return
		}
	}
	t.Fatal("selected Agent is outside the visible panel rows")
}

func TestAgentsOverviewShowsFinishedChildrenAndOpensDuringTurn(t *testing.T) {
	model, _ := newPanelModel(t)
	model.toggleActivity()
	model.activity.Agents[1].State = orchestration.AgentFailed
	model.activity.Agents[1].Activity = "failed"
	model.activity.Agents[1].Result = &orchestration.AgentResult{Status: orchestration.ResultFailed, Error: "model connection lost"}
	model.activity.Agents[2].State = orchestration.AgentCanceled
	model.activity.Agents[2].InitialPrompt = "Review chapter two\nand list errors"
	model.busy = true
	model.resize(80, 24)
	view := ansi.Strip(model.View())
	if !strings.Contains(view, "Child agents 2 · 1 failed · 1 canceled") {
		t.Fatalf("finished Agents disappeared from the overview:\n%s", view)
	}
	model.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, Y: model.headerHeight() + 1, X: 2})
	if !model.showActivity || !model.busy {
		t.Fatal("activity click did not open the panel during the turn")
	}
	panel := ansi.Strip(model.renderActivityPanel(model.width - 2))
	for _, expected := range []string{"model connection lost", "Review chapter two and list errors"} {
		if !strings.Contains(panel, expected) {
			t.Fatalf("panel missing %q:\n%s", expected, panel)
		}
	}
	model.toggleActivity()
	model.input.SetValue("/agents")
	model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !model.showActivity || !model.busy || model.input.Value() != "" {
		t.Fatal("/agents did not open the panel without sending another turn")
	}
	model.toggleActivity()
	model.showSessionInfo = true
	model.input.SetValue(strings.Repeat("draft ", 20))
	model.resize(24, 8)
	if view := ansi.Strip(model.View()); !strings.Contains(view, "Agents 2") || !strings.Contains(view, "Ctrl+G") || lipgloss.Height(view) != 8 {
		t.Fatalf("small terminal lost the count or the frame layout:\n%s", view)
	}
}

func TestAgentRefreshKeepsSelectedIdentityAcrossStateChange(t *testing.T) {
	model, _ := newPanelModel(t)
	model.panelSelected = 1
	next := model.activity
	next.Agents[1].State = orchestration.AgentCompleted
	model.Update(activityRefreshMsg{snapshot: next})
	selected, ok := model.selectedAgent()
	if !ok || selected.ID != "agent-child" || selected.State != orchestration.AgentCompleted || model.panelSelected != 2 {
		t.Fatalf("selected agent = %+v index=%d", selected, model.panelSelected)
	}
}

func TestAgentsPanelInterruptsChildAfterConfirmation(t *testing.T) {
	model, control := newPanelModel(t)
	if command := pressRune(model, 'x'); command != nil || len(control.interrupted) != 0 {
		t.Fatal("root Agent was interruptible from the panel")
	}
	model.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	if command := pressRune(model, 'x'); command != nil {
		t.Fatal("interrupt ran without confirmation")
	}
	if !strings.Contains(model.panelHint(), "press x again") {
		t.Fatalf("panel hint = %q", model.panelHint())
	}
	runAction(t, model, pressRune(model, 'x'))
	if len(control.interrupted) != 1 || control.interrupted[0] != "agent-child" {
		t.Fatalf("interrupted = %v", control.interrupted)
	}
}

func TestAgentsPanelReviewsAndDiscardsMerges(t *testing.T) {
	model, control := newPanelModel(t)
	runAction(t, model, pressRune(model, 'r'))
	if len(control.reviewed) != 1 || control.reviewed[0] != "team-test" {
		t.Fatalf("reviewed = %v", control.reviewed)
	}
	for range model.activity.Agents {
		model.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	}
	if merge, ok := model.selectedMerge(); !ok || merge.ID != "merge-1" {
		t.Fatalf("selection did not reach the merge row: %d", model.panelSelected)
	}
	pressRune(model, 'd')
	runAction(t, model, pressRune(model, 'd'))
	if len(control.discarded) != 1 || control.discarded[0] != "merge-1" {
		t.Fatalf("discarded = %v", control.discarded)
	}
}

func TestAgentsPanelOpensChildTranscript(t *testing.T) {
	model, _ := newPanelModel(t)
	model.blocks = append(model.blocks, transcriptBlock{kind: blockUser, content: "root question"})
	model.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	command, _ := model.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	runAction(t, model, command)
	plain := ansi.Strip(model.transcriptContent)
	if model.viewer == nil || !strings.Contains(plain, "parser inspected") || strings.Contains(plain, "root question") {
		t.Fatalf("viewer did not replace the transcript: %q", plain)
	}
	model.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if model.viewer != nil || !strings.Contains(ansi.Strip(model.transcriptContent), "root question") {
		t.Fatal("Esc did not return to the root transcript")
	}
}

func TestAgentsPanelLeavesTypedTextToInput(t *testing.T) {
	model, control := newPanelModel(t)
	model.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if model.panelFocused {
		t.Fatal("Esc did not return focus to the input")
	}
	if _, handled := model.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}}); handled || len(control.reviewed) != 0 {
		t.Fatal("unfocused panel consumed a letter")
	}
}

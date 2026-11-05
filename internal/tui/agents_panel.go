package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

const agentActionTimeout = 5 * time.Second

// transcriptViewer shows a child Agent's transcript in place of the root's.
// Root turns keep updating model.blocks underneath it.
type transcriptViewer struct {
	agentID orchestration.AgentID
	blocks  []transcriptBlock
}

// isUndecidedMerge reports whether a merge still waits for a decision.
func isUndecidedMerge(state workspacecoord.MergeState) bool {
	return state == workspacecoord.MergePending || state == workspacecoord.MergeConflicted
}

// panelRows counts the selectable rows: Agents first, then merges.
func (model *Model) panelRows() int {
	return len(model.activity.Agents) + len(model.activity.Merges)
}

// clampPanelSelection keeps the selection on a row after a refresh.
func (model *Model) clampPanelSelection() {
	model.panelSelected = max(0, min(model.panelSelected, model.panelRows()-1))
}

// selectedAgent returns the Agent under the selection, if it is an Agent row.
func (model *Model) selectedAgent() (orchestration.AgentSnapshot, bool) {
	if model.panelSelected < 0 || model.panelSelected >= len(model.activity.Agents) {
		return orchestration.AgentSnapshot{}, false
	}
	return model.activity.Agents[model.panelSelected], true
}

// selectedMerge returns the merge under the selection, if it is a merge row.
func (model *Model) selectedMerge() (workspacecoord.MergeRequest, bool) {
	index := model.panelSelected - len(model.activity.Agents)
	if index < 0 || index >= len(model.activity.Merges) {
		return workspacecoord.MergeRequest{}, false
	}
	return model.activity.Merges[index], true
}

// handleViewerKey closes the transcript viewer on Esc.
func (model *Model) handleViewerKey(message tea.KeyMsg) (tea.Cmd, bool) {
	if model.viewer == nil || message.Type != tea.KeyEsc {
		return nil, false
	}
	model.closeTranscriptViewer()
	return nil, true
}

// handlePanelKey runs /agents panel actions while the panel has focus. Keys
// that are not panel actions return the focus to the input.
func (model *Model) handlePanelKey(message tea.KeyMsg) (tea.Cmd, bool) {
	// Panel keys are letters; once the user writes, they belong to the input.
	if !model.showActivity || !model.panelFocused || model.input.Value() != "" {
		return nil, false
	}
	confirm := model.panelConfirm
	model.panelConfirm = ""
	switch message.Type {
	case tea.KeyUp:
		model.panelSelected = max(0, model.panelSelected-1)
	case tea.KeyDown:
		model.panelSelected = max(0, min(model.panelSelected+1, model.panelRows()-1))
	case tea.KeyEnter:
		return model.openSelectedTranscript(), true
	case tea.KeyEsc:
		model.panelFocused = false
	case tea.KeyRunes:
		if len(message.Runes) != 1 {
			model.panelFocused = false
			return nil, false
		}
		switch message.Runes[0] {
		case 'o':
			return model.openSelectedTranscript(), true
		case 'x':
			return model.interruptSelected(confirm), true
		case 'r':
			return model.reviewMerges(), true
		case 'd':
			return model.discardSelected(confirm), true
		default:
			model.panelFocused = false
			return nil, false
		}
	default:
		return nil, false
	}
	model.refreshTranscript(false)
	return nil, true
}

// notice appends a system or failure line to the transcript and scrolls to it.
func (model *Model) notice(kind blockKind, content string) {
	model.blocks = append(model.blocks, transcriptBlock{kind: kind, content: content})
	model.refreshTranscript(true)
}

// interruptSelected cancels the selected child Agent. The first key press
// asks for confirmation; the second one interrupts.
func (model *Model) interruptSelected(confirm string) tea.Cmd {
	agent, ok := model.selectedAgent()
	switch {
	case !ok:
		model.notice(blockSystem, "Select an Agent to interrupt")
		return nil
	case agent.ParentAgentID == "":
		model.notice(blockSystem, "Use Ctrl+C to stop the root Agent")
		return nil
	case isFinishedAgent(agent.State):
		model.notice(blockSystem, "Agent "+shortID(string(agent.ID))+" has already finished")
		return nil
	case model.agentControl == nil:
		model.notice(blockFailure, "Agent control is unavailable")
		return nil
	}
	action := "interrupt " + string(agent.ID)
	if confirm != action {
		model.panelConfirm = action
		model.refreshTranscript(false)
		return nil
	}
	control, ctx := model.agentControl, model.ctx
	return func() tea.Msg {
		actionContext, cancel := context.WithTimeout(ctx, agentActionTimeout)
		defer cancel()
		err := control.Interrupt(actionContext, agent.ID)
		return agentActionMsg{notice: "Interrupt requested for Agent " + shortID(string(agent.ID)), err: err}
	}
}

// reviewMerges asks for a decision on the merges that wait for one again.
func (model *Model) reviewMerges() tea.Cmd {
	if model.agentControl == nil {
		model.notice(blockFailure, "Agent control is unavailable")
		return nil
	}
	if model.undecidedMerges() == 0 {
		model.notice(blockSystem, "No merges wait for a decision")
		return nil
	}
	control, teamID := model.agentControl, model.teamID
	return func() tea.Msg {
		return agentActionMsg{notice: "Merge review started", err: control.ReviewMerges(teamID)}
	}
}

// discardSelected rejects the selected merge candidate after confirmation.
func (model *Model) discardSelected(confirm string) tea.Cmd {
	merge, ok := model.selectedMerge()
	switch {
	case !ok:
		model.notice(blockSystem, "Select a merge to discard")
		return nil
	case !isUndecidedMerge(merge.State):
		model.notice(blockSystem, "Merge "+shortID(string(merge.ID))+" is already "+string(merge.State))
		return nil
	case model.agentControl == nil:
		model.notice(blockFailure, "Agent control is unavailable")
		return nil
	}
	action := "discard " + string(merge.ID)
	if confirm != action {
		model.panelConfirm = action
		model.refreshTranscript(false)
		return nil
	}
	control, ctx := model.agentControl, model.ctx
	return func() tea.Msg {
		actionContext, cancel := context.WithTimeout(ctx, agentActionTimeout)
		defer cancel()
		_, err := control.DiscardMerge(actionContext, merge.ID, "candidate discarded by the user")
		return agentActionMsg{notice: "Merge " + shortID(string(merge.ID)) + " discarded", err: err}
	}
}

// finishAgentAction reports an action and refreshes the panel at once.
func (model *Model) finishAgentAction(message agentActionMsg) tea.Cmd {
	if message.err != nil {
		model.notice(blockFailure, message.err.Error())
	} else {
		model.notice(blockSystem, message.notice)
	}
	return model.startActivityRefresh()
}

// openSelectedTranscript loads the selected child Agent's transcript.
func (model *Model) openSelectedTranscript() tea.Cmd {
	agent, ok := model.selectedAgent()
	switch {
	case !ok:
		model.notice(blockSystem, "Select an Agent to open its transcript")
		return nil
	case agent.ParentAgentID == "":
		model.closeTranscriptViewer()
		return nil
	case model.transcripts == nil || agent.SessionID == "":
		model.notice(blockFailure, "Agent transcripts are unavailable")
		return nil
	}
	reader, ctx := model.transcripts, model.ctx
	return func() tea.Msg {
		loadContext, cancel := context.WithTimeout(ctx, agentActionTimeout)
		defer cancel()
		messages, err := reader.LoadTranscript(loadContext, agent.SessionID)
		return agentTranscriptMsg{agentID: agent.ID, messages: messages, err: err}
	}
}

// openTranscriptViewer replaces the visible transcript with a child's.
func (model *Model) openTranscriptViewer(message agentTranscriptMsg) {
	if message.err != nil {
		model.notice(blockFailure, fmt.Sprintf("load transcript of Agent %s: %v", shortID(string(message.agentID)), message.err))
		return
	}
	title := "Transcript of Agent " + string(message.agentID) + " · Esc returns to the conversation"
	blocks := append([]transcriptBlock{{kind: blockSystem, content: title}}, blocksFromHistory(message.messages)...)
	if len(blocks) == 1 {
		blocks = append(blocks, transcriptBlock{kind: blockSystem, content: "The Agent has no messages yet"})
	}
	model.viewer = &transcriptViewer{agentID: message.agentID, blocks: blocks}
	model.panelFocused = false
	model.selection = transcriptSelection{}
	model.refreshTranscript(true)
}

// closeTranscriptViewer returns to the root transcript.
func (model *Model) closeTranscriptViewer() {
	if model.viewer == nil {
		return
	}
	model.viewer = nil
	model.selection = transcriptSelection{}
	model.refreshTranscript(true)
}

// displayBlocks returns the blocks the transcript area shows.
func (model *Model) displayBlocks() []transcriptBlock {
	if model.viewer != nil {
		return model.viewer.blocks
	}
	return model.blocks
}

// panelHint lists the panel keys, or the pending confirmation.
func (model *Model) panelHint() string {
	if action, target, ok := strings.Cut(model.panelConfirm, " "); ok {
		key := "x"
		if action == "discard" {
			key = "d"
		}
		return "press " + key + " again to " + action + " " + shortID(target)
	}
	return "↑↓ select  Enter transcript  x interrupt  r review  d discard  Esc input"
}

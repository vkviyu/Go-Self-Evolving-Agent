package tui

import (
	"context"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

const rootInputPlaceholder = "Ask GSEA"

// transcriptViewer holds the selected child's display state. Root turns keep
// updating model.blocks independently of this view.
type transcriptViewer struct {
	agentID   orchestration.AgentID
	sessionID session.ID
	blocks    []transcriptBlock
	cancel    context.CancelFunc
	loaded    bool
	revision  uint64
	finished  bool
	version   uint64
	loadError bool
}

type transcriptReadingState struct {
	position transcriptPosition
	follow   bool
}

type transcriptRevisionReader interface {
	TranscriptRevision(context.Context, session.ID) (uint64, error)
}

// agentTranscriptMsg identifies the particular selection that started a read.
// A response for an earlier selection can arrive even after cancellation.
type agentTranscriptMsg struct {
	generation uint64
	revision   uint64
	finished   bool
	version    uint64
	unchanged  bool
	messages   []*schema.Message
	err        error
}

func (model *Model) openAgentTranscript(agent orchestration.AgentSnapshot) tea.Cmd {
	if model.approvalPending != nil {
		return nil
	}
	if agent.ParentAgentID == "" {
		model.closeTranscriptViewer()
		return nil
	}
	model.closeToolInspector()
	model.panelFocused = false
	if model.viewer == nil {
		model.rootReading = transcriptReadingState{position: model.readingPosition(), follow: model.autoScroll}
	} else {
		if model.viewer.agentID == agent.ID && model.viewer.sessionID == agent.SessionID {
			return model.loadAgentTranscript(agent)
		}
		if model.viewer.cancel != nil {
			model.viewer.cancel()
		}
	}

	state := transcriptBlock{kind: blockSystem, content: "Loading Agent transcript…"}
	switch {
	case model.transcripts == nil:
		state = transcriptBlock{kind: blockFailure, content: "Agent transcript is unavailable"}
	case agent.SessionID == "":
		state.content = "The Agent has no messages yet"
	}
	model.viewerGeneration++
	model.viewer = &transcriptViewer{agentID: agent.ID, sessionID: agent.SessionID, blocks: []transcriptBlock{
		{kind: blockSystem, content: "Transcript of Agent " + string(agent.ID) + " · Esc returns to root"}, state,
	}}
	model.input.Placeholder = "Ask root Agent"
	model.selection = transcriptSelection{}
	model.autoScroll = true
	model.resizeInput()
	return model.loadAgentTranscript(agent)
}

// loadAgentTranscript permits one read in flight for the active viewer.
func (model *Model) loadAgentTranscript(agent orchestration.AgentSnapshot) tea.Cmd {
	viewer, reader := model.viewer, model.transcripts
	if viewer == nil || reader == nil || viewer.sessionID == "" || viewer.cancel != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(model.ctx, agentActionTimeout)
	viewer.cancel = cancel
	known, previous, id := viewer.loaded, viewer.revision, viewer.sessionID
	generation, version, finished := model.viewerGeneration, agent.Version, isFinishedAgent(agent.State)
	return func() tea.Msg {
		defer cancel()
		message := agentTranscriptMsg{generation: generation, finished: finished, version: version}
		if revisions, ok := reader.(transcriptRevisionReader); ok {
			// Sampling before the read allows a racing append to cause an extra
			// refresh and never marks unseen events as already displayed.
			message.revision, message.err = revisions.TranscriptRevision(ctx, id)
			if message.err != nil {
				return message
			}
			if known && message.revision == previous {
				message.unchanged = true
				return message
			}
		}
		message.messages, message.err = reader.LoadTranscript(ctx, id)
		return message
	}
}

// refreshAgentTranscript follows only the selected child. A cheap revision
// check also catches events that reach the store after a terminal snapshot.
func (model *Model) refreshAgentTranscript() tea.Cmd {
	viewer := model.viewer
	if viewer == nil || viewer.cancel != nil {
		return nil
	}
	_, checksRevision := model.transcripts.(transcriptRevisionReader)
	for _, agent := range model.activity.Agents {
		if agent.ID != viewer.agentID {
			continue
		}
		if agent.SessionID != "" && agent.SessionID != viewer.sessionID {
			return model.openAgentTranscript(agent)
		}
		if checksRevision || !isFinishedAgent(agent.State) || !viewer.finished || agent.Version != viewer.version {
			return model.loadAgentTranscript(agent)
		}
		break
	}
	return nil
}

func (model *Model) applyAgentTranscript(message agentTranscriptMsg) {
	viewer := model.viewer
	if viewer == nil || message.generation != model.viewerGeneration {
		return
	}
	viewer.cancel = nil
	hadError := viewer.loadError
	if hadError {
		viewer.blocks = viewer.blocks[:len(viewer.blocks)-1]
		viewer.loadError = false
	}
	if message.err != nil {
		if !viewer.loaded {
			viewer.blocks = viewer.blocks[:1]
		}
		viewer.blocks = append(viewer.blocks, transcriptBlock{kind: blockFailure, content: fmt.Sprintf("Load Agent transcript: %v", message.err)})
		viewer.loadError = true
		model.refreshTranscript(false)
		return
	}

	viewer.finished, viewer.version = message.finished, message.version
	if message.unchanged {
		if hadError {
			model.refreshTranscript(false)
		}
		return
	}
	viewer.loaded, viewer.revision = true, message.revision
	blocks := blocksFromHistory(message.messages)
	// A refreshed child projection can insert or remove earlier messages.
	// Never leave a detail page pointing at a different call in that slot.
	if view := model.toolInspector; view != nil {
		for _, index := range view.indices {
			if index < 1 || index > len(blocks) || !sameTranscriptBlock(&viewer.blocks[index], &blocks[index-1]) {
				model.closeToolInspector()
				break
			}
		}
	}
	reuseTranscriptBlocks(viewer.blocks[1:], blocks)
	if len(blocks) == 0 {
		blocks = append(blocks, transcriptBlock{kind: blockSystem, content: "The Agent has no messages yet"})
	}
	viewer.blocks = append(viewer.blocks[:1], blocks...)
	model.refreshTranscript(false)
}

// Reuse rendered blocks whose history projection has not changed. Keep the
// tool disclosure even when a later event adds its result.
func reuseTranscriptBlocks(previous, next []transcriptBlock) {
	for index := range min(len(previous), len(next)) {
		old, block := &previous[index], &next[index]
		if !sameTranscriptBlock(old, block) {
			continue
		}
		if old.toolResult == block.toolResult {
			*block = *old
		}
	}
}

func sameTranscriptBlock(a, b *transcriptBlock) bool {
	return a.kind == b.kind && a.content == b.content && a.toolCallID == b.toolCallID && a.toolName == b.toolName && a.toolArguments == b.toolArguments
}

func (model *Model) closeTranscriptViewer() {
	if model.viewer == nil {
		return
	}
	model.closeToolInspector()
	if model.viewer.cancel != nil {
		model.viewer.cancel()
	}
	model.viewer = nil
	model.input.Placeholder = rootInputPlaceholder
	model.selection = transcriptSelection{}
	model.autoScroll = model.rootReading.follow
	model.resizeInput()
	if !model.autoScroll {
		model.viewport.GotoTop()
		model.restoreReadingPosition(model.rootReading.position)
	}
}

func (model *Model) displayBlocks() []transcriptBlock {
	if model.viewer != nil {
		return model.viewer.blocks
	}
	return model.blocks
}

// Root work should not move the reading position inside a child transcript.
func (model *Model) refreshRootTranscript(forceBottom bool) {
	if model.viewer == nil {
		model.refreshTranscript(forceBottom)
	}
}

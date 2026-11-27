package tui

import (
	"errors"

	tea "charm.land/bubbletea/v2"
	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateprocess"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

func startupPhaseLabel(phase StartupPhase) string {
	switch phase {
	case StartupModelInitialization:
		return "Model initialization"
	case StartupRootOwner:
		return "Root / owner"
	case StartupRuntimeFactory:
		return "Runtime factory"
	case StartupSupervisor:
		return "Supervisor recovery"
	case StartupSessionMailbox:
		return "Session / mailbox"
	case StartupMaintenance:
		return "Session maintenance"
	case StartupTranscript:
		return "Loading chat transcript"
	default:
		return ""
	}
}

func (model *Model) applyStartup(message StartupMsg) tea.Cmd {
	if model.startup == nil {
		return nil
	}
	state := message.State
	if message.Closed && !state.Ready && state.Err == nil {
		state.Err = errors.New("startup channel closed before runtime became ready")
	}
	if state.Err != nil || state.Ready {
		// Terminal messages end the single startup waiter chain. In particular,
		// never schedule another receive on a closed channel.
		model.startup = nil
		model.startupStorage = nil
		if state.Err != nil {
			model.startupErr = state.Err
			model.startupOperation = nil
			if model.turnCancel != nil {
				model.turnCancel()
				model.turnCancel = nil
			}
			model.busy = false
			model.turnEvents = nil
			model.stage = "Runtime startup failed"
			model.notice(blockFailure, "Runtime startup failed: "+state.Err.Error())
			return nil
		}

		model.teamID = state.TeamID
		model.activityProvider = state.Activity
		model.agentControl = state.Agents
		model.transcripts = state.Transcripts
		model.hydrateStartupHistory(state.RootTranscript)
		model.stage = "Ready"
		var operation tea.Cmd
		if queued := model.startupOperation; queued != nil {
			model.startupOperation = nil
			model.stage = model.startupStage
			model.resetProgress()
			operation = tea.Batch(
				queued(model.turnContext, model.turnEvents),
				waitForTurnEvent(model.ctx, model.turnEvents),
			)
		}
		return tea.Batch(operation,
			model.startActivityRefresh(), model.scheduleBackgroundCheck())
	}
	if state.Phase != 0 && state.Phase != model.startupPhase {
		model.startupPhase = state.Phase
		model.startupPhaseAt = state.At
	}
	if event := state.Storage; event != nil {
		if event.Stage == ateprocess.StageDone {
			delete(model.startupStorage, event.BatchID)
		} else {
			if model.startupStorage == nil {
				model.startupStorage = make(map[uint64]storageProgressMsg)
			}
			previous, exists := model.startupStorage[event.BatchID]
			if !exists || previous.progress.Stage != event.Stage {
				model.startupStorage[event.BatchID] = storageProgressMsg{progress: *event}
			}
		}
	}
	return waitForStartup(model.ctx, model.startup)
}

func (model *Model) hydrateStartupHistory(history []*schema.Message) {
	model.closeToolInspector()
	blocks := blocksFromHistory(history)
	// Recovered children can request approval as soon as setup releases them.
	// Preserve live block indices while prepending durable history.
	for _, index := range []*int{&model.approvalBlock, &model.activeAssistant, &model.lastAssistantMessage, &model.activeReasoning, &model.backgroundBlock} {
		if *index >= 0 {
			*index += len(blocks)
		}
	}
	model.blocks = append(blocks, model.blocks...)
	var inputs []string
	for _, message := range history {
		if message == nil {
			continue
		}
		if message.Role == schema.User && !isBackgroundInput(message.Content) {
			inputs = append(inputs, message.Content)
		}
		if message.Role == schema.Assistant && message.ResponseMeta != nil && message.ResponseMeta.Usage != nil {
			model.lastUsage = tokenusage.FromMessage(message)
		}
	}
	model.inputHistory = append(inputs, model.inputHistory...)
	model.historyIndex = len(model.inputHistory)
	model.sessionID = string(model.conversation.ID())
	model.completedTurns = model.conversation.CompletedTurns()
	if !model.startupTitleExplicit && len(inputs) > 0 {
		model.sessionTitle = singleLinePreview(inputs[0], 80)
	}
	if source, ok := model.conversation.(ContextConversation); ok {
		model.contextUsage = source.ContextUsage()
	}
	if model.startupOperation != nil && model.startupStage == "Starting request" && len(model.inputHistory) > 0 {
		if source, ok := model.conversation.(pendingContextConversation); ok {
			model.contextUsage = source.ContextUsageFor(model.inputHistory[len(model.inputHistory)-1])
		}
	}
	model.resizeInput()
	model.refreshRootTranscript(true)
}

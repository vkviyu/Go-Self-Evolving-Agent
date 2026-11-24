package tui

import (
	"context"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
)

var (
	selectAllInputBinding = key.NewBinding(key.WithKeys("ctrl+a", "alt+a"))
	copyInputBinding      = key.NewBinding(key.WithKeys("ctrl+c", "alt+c"))
	toggleToolBinding     = key.NewBinding(key.WithKeys("ctrl+o", "alt+o"))
	toggleActivityBinding = key.NewBinding(key.WithKeys("ctrl+g", "alt+g"))
	toggleInfoBinding     = key.NewBinding(key.WithKeys("alt+i"))
	jumpQuestionBinding   = key.NewBinding(key.WithKeys("alt+p"))
)

func (model *Model) handleKey(message tea.KeyMsg) (tea.Cmd, bool) {
	if model.approvalPending != nil {
		switch message.Type {
		case tea.KeyEnter:
			return model.resolveApproval(model.approvalChoice == approvalAllow), true
		case tea.KeyEsc, tea.KeyCtrlC:
			return model.resolveApproval(false), true
		case tea.KeyUp, tea.KeyLeft:
			model.approvalChoice = approvalAllow
			model.refreshTranscript(false)
			return nil, true
		case tea.KeyDown, tea.KeyRight:
			model.approvalChoice = approvalReject
			model.refreshTranscript(false)
			return nil, true
		case tea.KeyHome:
			model.approvalChoice = approvalAllow
			model.refreshTranscript(false)
			return nil, true
		case tea.KeyEnd:
			model.approvalChoice = approvalReject
			model.refreshTranscript(false)
			return nil, true
		default:
			return nil, true
		}
	}
	if model.toolInspector != nil && model.handleToolInspectionKey(message) {
		return nil, true
	}
	if command, handled := model.handleViewerKey(message); handled {
		return command, true
	}
	if command, handled := model.handlePanelKey(message); handled {
		return command, true
	}
	if !model.busy && key.Matches(message, selectAllInputBinding) {
		model.selectAllInput()
		return nil, true
	}
	if key.Matches(message, toggleToolBinding) {
		model.toggleToolDetails()
		return nil, true
	}
	if key.Matches(message, toggleActivityBinding) {
		return model.toggleActivity(), true
	}
	if key.Matches(message, toggleInfoBinding) {
		model.toggleSessionInfo()
		return nil, true
	}
	if key.Matches(message, jumpQuestionBinding) {
		model.jumpToQuestion()
		return nil, true
	}
	if key.Matches(message, copyInputBinding) {
		if selected := model.selectedInputText(); selected != "" {
			return copyToClipboard(selected, strings.Count(selected, "\n")+1), true
		}
	}
	switch message.Type {
	case tea.KeyCtrlC:
		// copyInputBinding above already copied any selected input text.
		if model.busy {
			if model.startupOperation != nil {
				model.startupOperation = nil
				model.turnCancel()
				model.turnCancel = nil
				model.busy = false
				model.wakeSuppressed = true
				model.stage = "Starting runtime"
				model.notice(blockSystem, "Queued request cancelled")
				return nil, true
			}
			if model.turnCancel != nil && !model.cancelling {
				model.cancelling = true
				model.stage = "Cancelling"
				// The user stopped the Agent; it must not wake itself again
				// before they speak.
				model.wakeSuppressed = true
				model.turnCancel()
			}
			return nil, true
		}
		if model.input.Value() != "" {
			model.input.Reset()
			model.clearInputSelection()
			model.updateCommandSuggestions()
			model.memorySuggestions = nil
			model.memorySuggestionItems = nil
			model.memorySuggestionIndex = 0
			model.resizeInput()
			return nil, true
		}
		return tea.Quit, true
	case tea.KeyCtrlD:
		if !model.busy && model.input.Value() == "" {
			return tea.Quit, true
		}
	case tea.KeyEnter:
		if len(model.memorySuggestions) > 0 {
			model.insertMemorySuggestion(model.memorySuggestions[model.memorySuggestionIndex])
			return nil, true
		}
		if model.busy {
			switch strings.ToLower(strings.TrimSpace(model.input.Value())) {
			case "/agents", "/activity":
			default:
				return nil, true
			}
		}
		return model.submit(), true
	case tea.KeyPgUp, tea.KeyPgDown:
		if model.ready {
			var command tea.Cmd
			model.viewport, command = model.viewport.Update(message)
			if message.Type == tea.KeyPgUp {
				model.autoScroll = false
			} else if model.viewport.AtBottom() {
				model.autoScroll = true
			}
			return command, true
		}
	case tea.KeyUp:
		if len(model.memorySuggestions) > 0 {
			model.memorySuggestionIndex = (model.memorySuggestionIndex - 1 + len(model.memorySuggestions)) % len(model.memorySuggestions)
			return nil, true
		}
		if model.inputDisplayHeight() == 1 && model.recallPrevious() {
			return nil, true
		}
	case tea.KeyDown:
		if len(model.memorySuggestions) > 0 {
			model.memorySuggestionIndex = (model.memorySuggestionIndex + 1) % len(model.memorySuggestions)
			return nil, true
		}
		if model.inputDisplayHeight() == 1 && model.recallNext() {
			return nil, true
		}
	case tea.KeyTab:
		if len(model.memorySuggestions) > 0 {
			model.insertMemorySuggestion(model.memorySuggestions[model.memorySuggestionIndex])
			return nil, true
		}
		if len(model.commandSuggestions) > 0 {
			model.input.SetValue(model.commandSuggestions[0])
			model.clearInputSelection()
			model.updateCommandSuggestions()
			model.resizeInput()
			return nil, true
		}
	}
	return nil, false
}

func (model *Model) submit() tea.Cmd {
	input := strings.TrimSpace(model.input.Value())
	if input == "" {
		return nil
	}
	if command, handled := model.handleCommand(input); handled {
		model.input.Reset()
		model.clearInputSelection()
		model.updateCommandSuggestions()
		model.memorySuggestions = nil
		model.memorySuggestionItems = nil
		model.memorySuggestionIndex = 0
		model.resizeInput()
		if command == nil {
			return func() tea.Msg { return nil }
		}
		return command
	}

	model.closeTranscriptViewer()
	titleCommand := model.showUserInput(input)
	if contextConversation, ok := model.conversation.(pendingContextConversation); ok {
		model.contextUsage = contextConversation.ContextUsageFor(input)
	}
	model.resetWakeBudget()
	model.inputHistory = append(model.inputHistory, input)
	model.historyIndex = len(model.inputHistory)
	model.historyDraft = ""
	model.input.Reset()
	model.clearInputSelection()
	model.updateCommandSuggestions()
	model.memorySuggestions = nil
	model.memorySuggestionItems = nil
	model.memorySuggestionIndex = 0
	model.resizeInput()
	return tea.Batch(titleCommand, model.startOperation("Starting request", func(ctx context.Context, events chan<- tea.Msg) tea.Cmd {
		return runOperationCommand(ctx, func(turnCtx context.Context, sink agentruntime.EventSink) (agentruntime.Result, error) {
			return model.runTurnWithMemory(turnCtx, input, sink)
		}, events, model.ctx)
	}))
}

func (model *Model) showUserInput(input string) tea.Cmd {
	model.blocks = append(model.blocks, transcriptBlock{kind: blockUser, content: input})
	if model.sessionTitle == "" {
		model.sessionTitle = singleLinePreview(input, 80)
		return model.windowTitleCommand()
	}
	return nil
}

func (model *Model) startOperation(stage string, command func(context.Context, chan<- tea.Msg) tea.Cmd) tea.Cmd {
	if model.startupErr != nil {
		model.notice(blockFailure, "Runtime startup failed: "+model.startupErr.Error())
		return nil
	}
	model.busy = true
	model.cancelling = false
	model.stage = stage
	model.resetProgress()
	model.activeAssistant = -1
	model.lastAssistantMessage = -1
	model.activeReasoning = -1
	model.activeTools = make(map[string]int)
	model.refreshRootTranscript(true)

	model.turnContext, model.turnCancel = context.WithCancel(model.ctx)
	model.turnEvents = make(chan tea.Msg, 256)
	if model.startup != nil {
		model.stage = "Waiting for runtime startup"
		model.startupStage = stage
		model.startupOperation = command
		// Init owns the startup waiter and spinner. No turn event waiter or
		// command exists until the final attachment has hydrated history.
		return nil
	}
	// Init starts the only approval waiter: delegated Agents ask for approval
	// between root turns too, and a second waiter would split the queue.
	return tea.Batch(
		command(model.turnContext, model.turnEvents),
		waitForTurnEvent(model.ctx, model.turnEvents),
		model.spinner.Tick,
		model.startActivityRefresh(),
	)
}

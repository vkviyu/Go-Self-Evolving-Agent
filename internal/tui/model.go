package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentmanager"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/approval"
	"github.com/vkviyu/go-self-evolving-agent/internal/experience"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

const (
	minimumWidth       = 24
	minimumHeight      = 8
	maximumInputHeight = 6
)

// Model is the complete terminal UI state for one conversation.
type Model struct {
	ctx                  context.Context
	conversation         Conversation
	modelName            string
	provider             string
	workspace            string
	effort               string
	sessionID            string
	sessionTitle         string
	showSessionInfo      bool
	completedTurns       int
	initialResumeTurn    string
	startup              <-chan StartupState
	startupPhase         StartupPhase
	startupPhaseAt       time.Time
	startupStorage       map[uint64]storageProgressMsg
	startupErr           error
	startupOperation     func(context.Context, chan<- tea.Msg) tea.Cmd
	startupStage         string
	startupTitleExplicit bool

	viewport viewport.Model
	input    textarea.Model
	spinner  spinner.Model
	styles   styles
	markdown markdownRenderer

	blocks               []transcriptBlock
	width                int
	height               int
	ready                bool
	autoScroll           bool
	busy                 bool
	cancelling           bool
	stage                string
	stageStartedAt       time.Time
	storageProgress      map[uint64]storageProgressMsg
	showReasoning        bool
	activeAssistant      int
	lastAssistantMessage int
	activeReasoning      int
	activeTools          map[string]int
	approvalBroker       *approval.Broker
	teamID               orchestration.TeamID
	activityProvider     agentmanager.TeamSnapshotReader
	activity             activitySnapshot
	showActivity         bool
	activityLoading      bool
	// activitySequence names the only refresh tick that may load the next
	// snapshot; an immediate refresh supersedes the tick already scheduled.
	activitySequence int
	agentControl     AgentControl
	transcripts      TranscriptReader
	memoryBrowser    experience.MemoryBrowser
	// panelFocused routes keys to the /agents panel instead of the input.
	panelFocused  bool
	panelSelected int
	// panelConfirm holds the destructive action that waits for a second key.
	panelConfirm string
	// viewer replaces the root transcript with a child Agent's transcript.
	viewer           *transcriptViewer
	viewerGeneration uint64
	rootReading      transcriptReadingState
	// background is nil when the root Agent must not wake by itself.
	background BackgroundConversation
	// backgroundRunning marks the current operation as a wake turn.
	backgroundRunning bool
	backgroundBlock   int
	// wakeSuppressed stops wake turns until the next user input, after the
	// user canceled a turn.
	wakeSuppressed   bool
	consecutiveWakes int
	lastWake         time.Time
	approvalPending  *approval.Request
	// approvalQueue holds requests that arrived while another was shown;
	// parallel child Agents can ask at the same time.
	approvalQueue         []approval.Request
	approvalBlock         int
	approvalChoice        int
	turnContext           context.Context
	turnCancel            context.CancelFunc
	turnEvents            chan tea.Msg
	lastUsage             tokenusage.Snapshot
	contextUsage          agentruntime.ContextUsage
	toolInspector         *toolInspector
	transcriptContent     string
	transcriptLayout      []transcriptSpan
	queryAnchors          []queryAnchor
	selection             transcriptSelection
	inputSelection        inputTextSelection
	clipboardNotice       string
	inputScrollOffset     int
	inputLayoutValue      string
	inputLayoutWidth      int
	inputLayout           []inputDisplayLine
	inputLayoutReady      bool
	inputHistory          []string
	historyIndex          int
	historyDraft          string
	commandSuggestions    []string
	memorySuggestions     []string
	memorySuggestionItems []experience.MemoryResult
	memorySuggestionIndex int
	memoryQuery           string
	memorySequence        int
}

// NewModel constructs testable UI state without taking terminal control.
func NewModel(ctx context.Context, config Config) (*Model, error) {
	if config.Conversation == nil {
		return nil, errors.New("TUI requires a conversation")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	input := textarea.New()
	input.Placeholder = rootInputPlaceholder
	input.Prompt = "› "
	input.ShowLineNumbers = false
	input.CharLimit = 1 << 20
	input.MaxHeight = maximumInputHeight
	input.SetHeight(1)
	input.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("ctrl+j"))
	input.KeyMap.Paste = key.NewBinding(key.WithKeys("ctrl+v", "alt+v"))

	styles := defaultStyles()
	input.FocusedStyle.Prompt = styles.input
	input.FocusedStyle.CursorLine = lipgloss.NewStyle()
	input.FocusedStyle.Text = lipgloss.NewStyle()
	input.FocusedStyle.Placeholder = styles.metadata
	input.BlurredStyle = input.FocusedStyle

	spinnerModel := spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(styles.status))
	var history []*schema.Message
	completedTurns := 0
	if config.Startup == nil {
		history = config.Conversation.History()
		completedTurns = config.Conversation.CompletedTurns()
	}
	inputHistory := make([]string, 0, completedTurns)
	lastUsage := tokenusage.Snapshot{}
	for _, message := range history {
		if message == nil {
			continue
		}
		if message.Role == schema.User && !isBackgroundInput(message.Content) {
			inputHistory = append(inputHistory, message.Content)
		}
		if message.Role == schema.Assistant && message.ResponseMeta != nil && message.ResponseMeta.Usage != nil {
			lastUsage = tokenusage.FromMessage(message)
		}
	}
	model := &Model{
		ctx:                  ctx,
		conversation:         config.Conversation,
		modelName:            strings.TrimSpace(config.Model),
		provider:             strings.TrimSpace(config.Provider),
		workspace:            strings.TrimSpace(config.Workspace),
		effort:               strings.TrimSpace(config.ReasoningEffort),
		sessionID:            string(config.Conversation.ID()),
		sessionTitle:         singleLinePreview(config.SessionTitle, 80),
		completedTurns:       completedTurns,
		initialResumeTurn:    config.ResumeTurnID,
		startup:              config.Startup,
		startupTitleExplicit: strings.TrimSpace(config.SessionTitle) != "",
		viewport:             viewport.New(0, 0),
		input:                input,
		spinner:              spinnerModel,
		styles:               styles,
		blocks:               blocksFromHistory(history),
		autoScroll:           true,
		showReasoning:        true,
		activeAssistant:      -1,
		lastAssistantMessage: -1,
		activeReasoning:      -1,
		activeTools:          make(map[string]int),
		approvalBroker:       config.Approval,
		teamID:               config.TeamID,
		activityProvider:     config.Activity,
		agentControl:         config.Agents,
		transcripts:          config.Transcripts,
		memoryBrowser:        config.Memory,
		backgroundBlock:      -1,
		approvalBlock:        -1,
		lastUsage:            lastUsage,
		inputHistory:         inputHistory,
		historyIndex:         len(inputHistory),
		stage:                "Ready",
	}
	if model.startup != nil {
		model.stage = "Starting runtime"
		model.stageStartedAt = time.Now()
		model.completedTurns = 0
	}
	if model.sessionTitle == "" && len(inputHistory) > 0 {
		model.sessionTitle = singleLinePreview(inputHistory[0], 80)
	}
	if background, ok := config.Conversation.(BackgroundConversation); ok && config.AutoContinue {
		model.background = background
	}
	if contextConversation, ok := config.Conversation.(ContextConversation); ok {
		model.contextUsage = contextConversation.ContextUsage()
	}
	return model, nil
}

// Init schedules startup, input and background activity for the terminal event loop.
func (model *Model) Init() tea.Cmd {
	var resume tea.Cmd
	if model.initialResumeTurn != "" {
		resume = model.startResume(model.initialResumeTurn)
	}
	if model.startup != nil {
		return tea.Batch(
			model.input.Focus(), model.windowTitleCommand(), resume,
			waitForApproval(model.ctx, model.approvalBroker),
			waitForStartup(model.ctx, model.startup), model.spinner.Tick,
		)
	}
	return tea.Batch(
		model.input.Focus(), model.windowTitleCommand(), resume,
		waitForApproval(model.ctx, model.approvalBroker),
		model.startActivityRefresh(), model.scheduleBackgroundCheck(),
	)
}

// Update applies one terminal or runtime message and returns the commands needed for
// subsequent work.
func (model *Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message.(type) {
	case runtimeEventMsg, storageProgressMsg:
		return model, model.updateTurnFrame(message)
	}
	previousStage := model.stage
	defer func() {
		if model.stage != previousStage {
			model.stageStartedAt = time.Now()
		}
	}()
	var commands []tea.Cmd
	switch message := message.(type) {
	case StartupMsg:
		return model, model.applyStartup(message)
	case memorySearchMsg:
		if message.suggestions {
			model.applyMemorySuggestions(message)
		} else {
			model.applyMemorySearch(message)
		}
		return model, nil
	case recoveriesMsg:
		model.showRecoveries(message)
		return model, nil
	case tea.WindowSizeMsg:
		model.resize(message.Width, message.Height)
		return model, nil
	case tea.KeyMsg:
		if command, handled := model.handleKey(message); handled {
			return model, command
		}
	case tea.MouseMsg:
		if model.ready {
			if command, handled := model.handleHeaderMouse(message); handled {
				return model, command
			}
			if command, handled := model.handleActivityMouse(message); handled {
				return model, command
			}
			if command, handled := model.handleInputMouse(message); handled {
				return model, command
			}
			if command, handled := model.handleTranscriptMouse(message); handled {
				return model, command
			}
			var command tea.Cmd
			model.viewport, command = model.viewport.Update(message)
			if view := model.toolInspector; view != nil {
				if !view.detail && tea.MouseEvent(message).IsWheel() {
					top := model.viewport.YOffset
					view.selected = min(max(top, view.selected), min(len(view.indices)-1, top+model.viewport.Height-1))
					model.refreshTranscript(false)
				}
				return model, command
			}
			if message.Button == tea.MouseButtonWheelUp {
				model.autoScroll = false
			} else if message.Button == tea.MouseButtonWheelDown && model.viewport.AtBottom() {
				model.autoScroll = true
			}
			return model, command
		}
	case approvalRequestMsg:
		model.closeToolInspector()
		model.closeTranscriptViewer()
		model.applyApprovalRequest(message.request)
		model.refreshTranscript(true)
		return model, waitForApproval(model.ctx, model.approvalBroker)
	case turnDoneMsg:
		return model.finishTurn(message)
	case compressDoneMsg:
		return model.finishCompression(message)
	case spinner.TickMsg:
		if model.busy || model.startup != nil {
			var command tea.Cmd
			model.spinner, command = model.spinner.Update(message)
			commands = append(commands, command)
		}
	case clipboardResultMsg:
		if message.err != nil {
			model.clipboardNotice = "copy failed"
		} else {
			model.clipboardNotice = fmt.Sprintf("copied %d lines", message.lines)
		}
		model.refreshTranscript(false)
		return model, nil
	case activityRefreshMsg:
		model.applyActivitySnapshot(message.snapshot)
		model.resizeInput()
		return model, tea.Batch(model.scheduleActivityRefresh(), model.refreshAgentTranscript())
	case activityTickMsg:
		if message.sequence != model.activitySequence || model.activityProvider == nil {
			return model, nil
		}
		return model, model.startActivityRefresh()
	case agentActionMsg:
		return model, model.finishAgentAction(message)
	case agentTranscriptMsg:
		model.applyAgentTranscript(message)
		return model, nil
	case backgroundTickMsg:
		return model, model.checkBackground()
	case backgroundPendingMsg:
		return model, model.handleBackgroundPending(message)
	}

	previous := model.input.Value()
	var inputCommand tea.Cmd
	model.input, inputCommand = model.input.Update(message)
	if inputCommand != nil {
		commands = append(commands, inputCommand)
	}
	if model.input.Value() != previous {
		model.clearInputSelection()
		model.historyIndex = len(model.inputHistory)
		model.updateCommandSuggestions()
		if command := model.refreshMemorySuggestions(); command != nil {
			commands = append(commands, command)
		}
		model.resizeInput()
	} else {
		model.updateInputScroll()
	}
	return model, tea.Batch(commands...)
}

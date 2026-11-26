package tui

import (
	"context"
	"errors"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/approval"
	"github.com/vkviyu/go-self-evolving-agent/internal/conversation"
	"github.com/vkviyu/go-self-evolving-agent/internal/observation"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/render"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

// applyRuntimeEvent reports whether the visible transcript needs new layout.
// Status-only events still update the status bar through Model.View.
func (model *Model) applyRuntimeEvent(event agentruntime.Event) bool {
	switch event.Type {
	case agentruntime.EventMemoryReferenceLookupStarted:
		model.stage = "Reading referenced memory"
	case agentruntime.EventTurnStarting:
		model.stage = "Starting request"
	case agentruntime.EventTurnResuming:
		model.stage = "Recovering turn " + event.TurnID
	case agentruntime.EventConversationLockWaiting:
		model.stage = "Waiting for previous request"
	case agentruntime.EventConversationHistoryLoading:
		model.stage = "Loading conversation history"
	case agentruntime.EventTurnBuilding:
		model.stage = "Building request"
	case agentruntime.EventTurnStrategySelectionStarted:
		model.stage = "Loading Agent strategy"
	case agentruntime.EventTurnBackgroundReconciliationStarted:
		model.stage = "Reconciling Agent messages"
	case agentruntime.EventTurnSnapshotRetentionStarted:
		model.stage = "Saving tool snapshot"
	case agentruntime.EventTurnSessionSequenceReadStarted:
		model.stage = "Reading session state"
	case agentruntime.EventTurnSessionCreationStarted:
		model.stage = "Creating session"
	case agentruntime.EventTurnAgentConstructionStarted:
		model.stage = "Building Agent and tools"
	case agentruntime.EventTurnPersistenceStarted:
		model.stage = "Persisting turn"
	case agentruntime.EventAgentLoopStarting:
		model.stage = "Starting Agent loop"
	case agentruntime.EventModelPreparationStarted:
		model.stage = model.modelPreparationStage(event)
	case agentruntime.EventExperiencePreparationStarted:
		model.stage = "Preparing memory"
	case agentruntime.EventContextTokenCountingStarted:
		model.stage = "Counting context tokens"
	case agentruntime.EventFileReferenceInspectionStarted:
		model.stage = "Reading referenced files"
	case agentruntime.EventTurnPreparedPersistenceStarted:
		model.stage = "Saving prepared context"
	case agentruntime.EventExperienceSynchronizationStarted:
		model.stage = "Checking memory authorization"
	case agentruntime.EventContextCompactionStarted:
		model.stage = "Compacting context"
	case agentruntime.EventModelBudgetReservationStarted:
		model.stage = "Reserving model budget"
	case agentruntime.EventModelRequestPersistenceStarted:
		model.stage = "Persisting request"
	case agentruntime.EventModelDispatchPersistenceStarted:
		model.stage = "Recording model dispatch"
	case agentruntime.EventModelRequestStarted:
		model.stage = model.modelRequestStage(event)
	case agentruntime.EventModelResponseStarted:
		model.stage = model.modelResponseStage(event)
	case agentruntime.EventModelToolFormatRetry:
		model.stage = "Retrying rejected tool call"
		model.finishInterruptedModelOutput()
		model.blocks = append(model.blocks, transcriptBlock{kind: blockSystem, content: "Tool call format was rejected before execution. Retrying this model step."})
		return true
	case agentruntime.EventModelTimeoutRetry:
		model.stage = "Retrying timed-out model request"
		model.finishInterruptedModelOutput()
		model.blocks = append(model.blocks, transcriptBlock{kind: blockSystem, content: "Model request timed out. Retrying this model step."})
		return true
	case agentruntime.EventToolProposed:
		model.stage = toolStage("Preparing tool call", event)
	case agentruntime.EventToolPolicyChecking:
		model.stage = toolStage("Checking tool policy", event)
	case agentruntime.EventToolPolicyDecided:
		model.stage = toolStage("Tool policy checked", event)
	case agentruntime.EventToolApprovalRequested:
		model.stage = toolStage("Approval required", event)
	case agentruntime.EventToolApprovalWaiting:
		model.stage = toolStage("Waiting for approval", event)
	case agentruntime.EventToolApprovalResolved:
		if event.Approved {
			model.stage = toolStage("Approval granted", event)
		} else {
			model.stage = toolStage("Approval denied", event)
		}
	case agentruntime.EventToolExecutionStarted:
		model.stage = toolStage("Running", event)
	case agentruntime.EventToolExecutionSucceeded:
		model.stage = toolStage("Tool completed", event)
	case agentruntime.EventToolExecutionFailed:
		model.stage = toolStage("Tool failed", event)
	case agentruntime.EventToolOutcomeUnknown:
		model.stage = toolStage("Tool outcome unknown; inspect state", event)
	case agentruntime.EventTurnFinalizing:
		model.stage = "Finalizing"
	case agentruntime.EventAssistantReasoningDelta:
		model.stage = "Reasoning"
		if model.activeReasoning < 0 {
			model.blocks = append(model.blocks, transcriptBlock{kind: blockReasoning, pending: true})
			model.activeReasoning = len(model.blocks) - 1
			model.observeBlock(event, "reasoning")
		}
		model.blocks[model.activeReasoning].appendDelta(event.Delta)
		return model.showReasoning
	case agentruntime.EventAssistantTextDelta:
		model.stage = "Responding"
		if model.activeAssistant < 0 {
			model.blocks = append(model.blocks, transcriptBlock{kind: blockAssistant, pending: true})
			model.activeAssistant = len(model.blocks) - 1
			model.observeBlock(event, "text")
		}
		model.blocks[model.activeAssistant].appendDelta(event.Delta)
		return true
	case agentruntime.EventAssistantMessage:
		model.applyAssistantMessage(event.Message)
		return true
	case agentruntime.EventToolResult:
		model.applyToolResult(event.Message)
		return true
	}
	return false
}

func (model *Model) finishInterruptedModelOutput() {
	model.finishActiveStreams()
	model.activeAssistant = -1
	model.activeReasoning = -1
	model.lastAssistantMessage = -1
}

func (model *Model) finishActiveStreams() {
	for _, index := range []int{model.activeAssistant, model.activeReasoning} {
		if index >= 0 && index < len(model.blocks) {
			model.blocks[index].finishStream()
		}
	}
}

func (model *Model) observeBlock(event agentruntime.Event, kind string) {
	observation.Emit(model.turnContext, observation.Event{
		Component: "tui", Name: "transcript.block.started", Status: kind,
		Scope: observation.Scope{SessionID: model.sessionID, TurnID: event.TurnID},
	})
}

func toolStage(prefix string, event agentruntime.Event) string {
	if name := strings.TrimSpace(render.PlainText(event.ToolName)); name != "" {
		if prefix == "Running" {
			return prefix + " " + name
		}
		return prefix + " · " + name
	}
	return prefix
}

func (model *Model) modelPreparationStage(event agentruntime.Event) string {
	switch event.Purpose {
	case "experience":
		return "Preparing memory"
	case "compaction":
		return "Compacting context"
	case "verification":
		return "Preparing verification"
	default:
		return "Preparing context"
	}
}

func (model *Model) modelRequestStage(event agentruntime.Event) string {
	switch event.Purpose {
	case "experience":
		return "Preparing memory · requesting model"
	case "compaction":
		return "Compacting context · requesting model"
	case "verification":
		return "Verifying outcome · requesting model"
	default:
		return "Requesting model"
	}
}

func (model *Model) modelResponseStage(event agentruntime.Event) string {
	// The stream can be open before the provider sends model output.
	switch event.Purpose {
	case "experience":
		return "Preparing memory · waiting for model output"
	case "compaction":
		return "Compacting context · waiting for model output"
	case "verification":
		return "Verifying outcome · waiting for model output"
	default:
		return "Waiting for model output"
	}
}

// applyApprovalRequest shows request, or queues it while another request is
// awaiting a decision. Only root requests attach to the root's tool cards:
// a delegated Agent may reuse a call ID that the root is also running.
func (model *Model) applyApprovalRequest(request approval.Request) {
	if model.approvalPending != nil {
		model.approvalQueue = append(model.approvalQueue, request)
		return
	}
	model.approvalPending = &request
	model.approvalChoice = approvalAllow
	model.stage = "Waiting for approval"
	if index, exists := model.activeTools[request.Policy.CallID]; exists && request.Source.AgentID == "" {
		model.approvalBlock = index
		model.blocks[index].pending = true
		model.blocks[index].approvalState = approvalPending
		model.blocks[index].approvalReason = strings.TrimSpace(request.Decision.Reason)
		return
	}
	model.approvalBlock = len(model.blocks)
	arguments := string(request.Policy.NormalizedArg)
	model.blocks = append(model.blocks, transcriptBlock{
		kind:           blockApproval,
		content:        request.Policy.ToolName,
		toolName:       request.Policy.ToolName,
		toolArguments:  arguments,
		approvalReason: strings.TrimSpace(request.Decision.Reason),
		approvalSource: approvalSourceLabel(request.Source),
		pending:        true,
		approvalState:  approvalPending,
		// A request that proposes a diff, such as a snapshot merge, shows it
		// at once: the diff is what the user decides on.
		argumentsExpanded: hasDiffPayload(arguments),
	})
}

// approvalSourceLabel names a delegated Agent as "profile id · task"; the
// root Agent has no label.
func approvalSourceLabel(source approval.Source) string {
	if source.AgentID == "" {
		return ""
	}
	label := shortID(string(source.AgentID))
	if profile := strings.TrimSpace(source.Profile); profile != "" {
		label = profile + " " + label
	}
	if task := strings.TrimSpace(source.Task); task != "" {
		label += " · " + task
	}
	return label
}

func (model *Model) resolveApproval(approved bool) tea.Cmd {
	request := model.approvalPending
	if request == nil {
		return nil
	}
	if model.approvalBroker != nil {
		if err := model.approvalBroker.Resolve(request.ID, approved); err != nil {
			model.blocks = append(model.blocks, transcriptBlock{kind: blockFailure, content: err.Error()})
		}
	}
	if model.approvalBlock >= 0 && model.approvalBlock < len(model.blocks) {
		model.blocks[model.approvalBlock].pending = false
		if approved {
			model.blocks[model.approvalBlock].approvalState = approvalApproved
		} else {
			model.blocks[model.approvalBlock].approvalState = approvalRejected
		}
	}
	model.approvalPending = nil
	model.approvalBlock = -1
	model.approvalChoice = approvalAllow
	model.stage = "Ready"
	if model.busy {
		model.stage = "Running tool"
	}
	if len(model.approvalQueue) > 0 {
		next := model.approvalQueue[0]
		model.approvalQueue = model.approvalQueue[1:]
		model.applyApprovalRequest(next)
	}
	model.refreshTranscript(true)
	// The single approval waiter started by Init is already active. Do not
	// start a second consumer for the broker channel.
	return nil
}

func (model *Model) applyAssistantMessage(message *schema.Message) {
	if message == nil {
		return
	}
	if usage := tokenusage.FromMessage(message); usageAvailable(usage) {
		model.lastUsage = usage
	}
	// Deltas are a live preview. The complete message is authoritative and
	// may contain a suffix the last visible streaming chunk did not carry.
	model.reconcileAssistantContent(message.Content, -1)
	model.finishActiveStreams()
	model.lastAssistantMessage = model.activeAssistant
	model.activeAssistant = -1
	model.activeReasoning = -1
	if len(message.ToolCalls) == 0 {
		model.stage = "Responding"
	}
	for _, call := range message.ToolCalls {
		model.stage = "Running " + render.PlainText(call.Function.Name)
		model.blocks = append(model.blocks, transcriptBlock{
			kind:          blockTool,
			content:       call.Function.Name,
			toolName:      call.Function.Name,
			toolArguments: call.Function.Arguments,
			toolCallID:    call.ID,
			pending:       true,
		})
		model.activeTools[call.ID] = len(model.blocks) - 1
	}
	if len(message.ToolCalls) > 0 {
		// A tool prelude is not the final answer of a later model request.
		model.lastAssistantMessage = -1
	}
}

func (model *Model) reconcileAssistantContent(content string, completed int) {
	if content == "" {
		return
	}
	index := model.activeAssistant
	if index < 0 {
		index = completed
	}
	if index >= 0 && index < len(model.blocks) && model.blocks[index].kind == blockAssistant {
		model.blocks[index].content = content
		model.activeAssistant = index
		return
	}
	model.blocks = append(model.blocks, transcriptBlock{kind: blockAssistant, content: content})
	model.activeAssistant = len(model.blocks) - 1
}

func (model *Model) applyToolResult(message *schema.Message) {
	if message == nil {
		return
	}
	model.stage = "Processing tool result"
	if index, exists := model.activeTools[message.ToolCallID]; exists {
		model.blocks[index].pending = false
		model.blocks[index].toolResult = message.Content
		return
	}
	content := message.Content
	toolResult := ""
	if message.ToolName != "" {
		content = message.ToolName
		toolResult = message.Content
	}
	model.blocks = append(model.blocks, transcriptBlock{kind: blockTool, content: content, toolName: message.ToolName, toolResult: toolResult})
}

func (model *Model) finishTurn(message turnDoneMsg) (tea.Model, tea.Cmd) {
	model.busy = false
	model.cancelling = false
	if model.turnCancel != nil {
		model.turnCancel()
	}
	model.turnCancel = nil
	model.turnContext = nil
	model.turnEvents = nil
	for index := range model.blocks {
		model.blocks[index].finishStream()
	}
	wake := model.backgroundRunning
	if model.finishWake(message.err) {
		model.stage = "Ready"
		model.activeAssistant = -1
		model.lastAssistantMessage = -1
		model.activeReasoning = -1
		model.activeTools = make(map[string]int)
		model.refreshRootTranscript(false)
		return model, model.input.Focus()
	}

	if message.err != nil {
		kind, paused := orchestration.ExecutionStopKindOf(message.err)
		// Conversation implementations may return a fatal tool error directly,
		// without passing through the runtime's Eino failure boundary.
		var unknown *tool.OutcomeUnknownError
		if errors.As(message.err, &unknown) && unknown != nil {
			kind, paused = orchestration.ExecutionOutcomeUnknown, true
		}
		if errors.Is(message.err, context.Canceled) && kind != orchestration.ExecutionOutcomeUnknown {
			model.stage = "Cancelled"
		} else if !paused && (errors.Is(message.err, tool.ErrSnapshotHostChanged) ||
			errors.Is(message.err, conversation.ErrAmbiguousRecovery) || errors.Is(message.err, conversation.ErrNoForegroundRecovery)) {
			// The open session still accepts new input. A repeated shortcut
			// against the same blocked turn needs only one visible diagnostic.
			model.stage = "Ready"
			detail := message.err.Error()
			if n := len(model.blocks); n == 0 || model.blocks[n-1].kind != blockFailure || model.blocks[n-1].content != detail {
				model.blocks = append(model.blocks, transcriptBlock{kind: blockFailure, content: detail})
			}
		} else {
			model.stage = "Failed"
			detail := message.err.Error()
			if paused {
				model.stage = "Paused"
				if kind == orchestration.ExecutionOutcomeUnknown {
					detail += "\nInspect actual state before retrying. Pending persistence needs reconciliation."
					detail += "\n/recoveries only inspects interrupted work; it does not reconcile saved ATE appends."
					detail += "\nClose the runtime owning this session before reconciliation, then run: gsea session reconcile <session-id>."
				} else if _, canInspect := model.conversation.(RecoveryConversation); canInspect {
					detail += "\nUse /resume to continue the foreground turn or /recoveries to inspect interrupted work."
				}
			}
			model.blocks = append(model.blocks, transcriptBlock{kind: blockFailure, content: detail})
		}
	} else {
		model.stage = "Ready"
		// A wake turn answers Agent messages; it is not a user turn.
		if !wake {
			model.completedTurns++
		}
		if message.result.Final != nil {
			model.reconcileAssistantContent(message.result.Final.Content, model.lastAssistantMessage)
		}
		if usage := tokenusage.FromMessage(message.result.Final); usageAvailable(usage) {
			model.lastUsage = usage
		}
		model.refreshContextUsage()
		if message.result.ContextUsage != nil {
			model.contextUsage = *message.result.ContextUsage
		}
	}
	model.activeAssistant = -1
	model.lastAssistantMessage = -1
	model.activeReasoning = -1
	model.activeTools = make(map[string]int)
	model.refreshRootTranscript(false)
	return model, model.input.Focus()
}

func usageAvailable(usage tokenusage.Snapshot) bool {
	return usage.Input > 0 || usage.Cached > 0 || usage.Output > 0 || usage.Reasoning > 0 || usage.Total > 0
}

func (model *Model) finishCompression(message compressDoneMsg) (tea.Model, tea.Cmd) {
	model.busy = false
	model.cancelling = false
	if model.turnCancel != nil {
		model.turnCancel()
	}
	model.turnCancel = nil
	model.turnContext = nil
	model.turnEvents = nil
	if errors.Is(message.err, context.Canceled) {
		model.stage = "Cancelled"
		model.refreshRootTranscript(true)
		return model, model.input.Focus()
	}
	if message.err != nil {
		model.stage = "Failed"
		model.blocks = append(model.blocks, transcriptBlock{kind: blockFailure, content: message.err.Error()})
	} else {
		model.stage = "Ready"
		model.contextUsage = message.usage
		content := "Context is already compact"
		if message.changed {
			content = "Context compacted"
		}
		model.blocks = append(model.blocks, transcriptBlock{kind: blockSystem, content: content})
	}
	model.refreshRootTranscript(true)
	return model, model.input.Focus()
}

func (model *Model) refreshContextUsage() {
	if conversation, ok := model.conversation.(ContextConversation); ok {
		model.contextUsage = conversation.ContextUsage()
	}
}

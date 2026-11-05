package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/approval"
	"github.com/vkviyu/go-self-evolving-agent/internal/render"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

func (model *Model) applyRuntimeEvent(event agentruntime.Event) {
	switch event.Type {
	case agentruntime.EventGoalPhase:
		if event.GoalPhase == nil {
			return
		}
		phase := event.GoalPhase
		switch phase.Stage {
		case "executing":
			model.stage = fmt.Sprintf("Executing task · round %d/%d", phase.Round, phase.MaxRounds)
		case "verifying":
			model.stage = fmt.Sprintf("Verifying outcome · round %d/%d", phase.Round, phase.MaxRounds)
		case "retrying":
			model.stage = fmt.Sprintf("Continuing task · round %d/%d", phase.Round, phase.MaxRounds)
		case "stopped":
			model.stage = "Task " + phase.State
			if phase.State != "completed" {
				model.blocks = append(model.blocks, transcriptBlock{kind: blockSystem, content: "Task " + phase.State + ": " + phase.Reason})
			}
		}
	case agentruntime.EventAssistantReasoningDelta:
		model.stage = "Reasoning"
		if model.activeReasoning < 0 {
			model.blocks = append(model.blocks, transcriptBlock{kind: blockReasoning, pending: true})
			model.activeReasoning = len(model.blocks) - 1
		}
		model.blocks[model.activeReasoning].content += event.Delta
	case agentruntime.EventAssistantTextDelta:
		model.stage = "Responding"
		if model.activeAssistant < 0 {
			model.blocks = append(model.blocks, transcriptBlock{kind: blockAssistant, pending: true})
			model.activeAssistant = len(model.blocks) - 1
		}
		model.blocks[model.activeAssistant].content += event.Delta
	case agentruntime.EventAssistantMessage:
		model.applyAssistantMessage(event.Message)
	case agentruntime.EventToolResult:
		model.applyToolResult(event.Message)
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
	model.stage = "Approval required"
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
	model.lastAssistantMessage = model.activeAssistant
	model.activeAssistant = -1
	model.activeReasoning = -1
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
		model.blocks[index].pending = false
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
		model.blocks[index].pending = false
	}
	wake := model.backgroundRunning
	if model.finishWake(message.err) {
		model.stage = "Ready"
		model.activeAssistant = -1
		model.lastAssistantMessage = -1
		model.activeReasoning = -1
		model.activeTools = make(map[string]int)
		model.refreshTranscript(false)
		return model, model.input.Focus()
	}

	if message.err != nil {
		if errors.Is(message.err, context.Canceled) {
			model.stage = "Cancelled"
		} else {
			model.stage = "Failed"
			model.blocks = append(model.blocks, transcriptBlock{kind: blockFailure, content: message.err.Error()})
		}
	} else {
		model.stage = "Ready"
		if message.result.GoalState != "" && message.result.GoalState != "completed" {
			model.stage = "Task " + string(message.result.GoalState)
		}
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
	model.refreshTranscript(false)
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
	if errors.Is(message.err, context.Canceled) {
		model.stage = "Cancelled"
		model.refreshTranscript(true)
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
	model.refreshTranscript(true)
	return model, model.input.Focus()
}

func (model *Model) refreshContextUsage() {
	if conversation, ok := model.conversation.(ContextConversation); ok {
		model.contextUsage = conversation.ContextUsage()
	}
}

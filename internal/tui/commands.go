package tui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

var commandNames = []string{"/activity", "/agents", "/clear", "/compress", "/context", "/diff", "/exit", "/help", "/info", "/merge", "/question", "/reasoning", "/recoveries", "/resume", "/session"}

func (model *Model) handleCommand(input string) (tea.Cmd, bool) {
	fields := strings.Fields(input)
	if len(fields) > 0 && strings.EqualFold(fields[0], "/resume") {
		if len(fields) != 2 {
			model.recoveryFailure("usage: /resume <turn-id>")
			return nil, true
		}
		return model.startResume(fields[1]), true
	}
	switch strings.ToLower(input) {
	case "/info":
		model.toggleSessionInfo()
		return nil, true
	case "/question":
		model.jumpToQuestion()
		return nil, true
	case "/context":
		model.refreshContextUsage()
		model.notice(blockSystem, model.contextUsageDescription())
		return nil, true
	case "/recoveries":
		if source, ok := model.conversation.(RecoveryConversation); ok {
			return discoverRecoveries(model.ctx, source, true), true
		}
		model.recoveryFailure("recovery is unavailable")
		return nil, true
	case "/agents", "/activity":
		return model.openAgentsPanel(), true
	case "/merge", "/merges":
		model.appendMergeSummary()
		return nil, true
	case "/diff", "/diffs":
		found := false
		for index := range model.blocks {
			if model.blocks[index].kind != blockTool || !hasDiffPayload(model.blocks[index].toolResult) {
				continue
			}
			model.blocks[index].resultExpanded = true
			found = true
		}
		if !found {
			model.blocks = append(model.blocks, transcriptBlock{kind: blockSystem, content: "No inline code diffs in the current transcript"})
		}
		model.refreshTranscript(true)
		return nil, true
	case "/exit", "/quit":
		return tea.Quit, true
	case "/clear":
		model.blocks = nil
		model.refreshTranscript(true)
		return nil, true
	case "/compress":
		compressor, ok := model.conversation.(ContextConversation)
		if !ok {
			model.notice(blockFailure, "context compression is unavailable")
			return nil, true
		}
		model.busy = true
		model.stage = "Compacting context"
		model.refreshTranscript(true)
		model.turnContext, model.turnCancel = context.WithCancel(model.ctx)
		return tea.Batch(runCompressCommand(model.turnContext, compressor), model.spinner.Tick), true
	case "/session":
		model.notice(blockSystem, model.sessionID)
		return nil, true
	case "/reasoning":
		model.showReasoning = !model.showReasoning
		state := "hidden"
		if model.showReasoning {
			state = "visible"
		}
		model.notice(blockSystem, "Reasoning summaries "+state)
		return nil, true
	case "/help":
		model.notice(blockSystem, strings.Join(commandNames, "  "))
		return nil, true
	default:
		if strings.HasPrefix(input, "/") {
			model.notice(blockFailure, fmt.Sprintf("Unknown command %q", input))
			return nil, true
		}
		return nil, false
	}
}

func (model *Model) appendMergeSummary() {
	if len(model.activity.Merges) == 0 {
		model.notice(blockSystem, "No merge requests in the current Team")
		return
	}
	var summary strings.Builder
	for index, merge := range model.activity.Merges {
		if index > 0 {
			summary.WriteByte('\n')
		}
		summary.WriteString(string(merge.ID))
		summary.WriteString("  ")
		summary.WriteString(string(merge.State))
		if merge.ResolutionTaskID != "" {
			summary.WriteString("  resolution task ")
			summary.WriteString(string(merge.ResolutionTaskID))
		}
		if strings.TrimSpace(merge.Reason) != "" {
			summary.WriteString("\n  ")
			summary.WriteString(strings.TrimSpace(merge.Reason))
		}
	}
	model.notice(blockSystem, summary.String())
}

func (model *Model) recallPrevious() bool {
	if len(model.inputHistory) == 0 || model.historyIndex <= 0 {
		return false
	}
	if model.historyIndex == len(model.inputHistory) {
		model.historyDraft = model.input.Value()
	}
	model.historyIndex--
	model.input.SetValue(model.inputHistory[model.historyIndex])
	model.clearInputSelection()
	model.updateCommandSuggestions()
	model.resizeInput()
	return true
}

func (model *Model) recallNext() bool {
	if model.historyIndex >= len(model.inputHistory) {
		return false
	}
	model.historyIndex++
	if model.historyIndex == len(model.inputHistory) {
		model.input.SetValue(model.historyDraft)
	} else {
		model.input.SetValue(model.inputHistory[model.historyIndex])
	}
	model.clearInputSelection()
	model.updateCommandSuggestions()
	model.resizeInput()
	return true
}

func (model *Model) updateCommandSuggestions() {
	value := strings.ToLower(strings.TrimSpace(model.input.Value()))
	model.commandSuggestions = nil
	if !strings.HasPrefix(value, "/") || strings.Contains(value, "\n") {
		return
	}
	for _, command := range commandNames {
		if strings.HasPrefix(command, value) && command != value {
			model.commandSuggestions = append(model.commandSuggestions, command)
		}
	}
}

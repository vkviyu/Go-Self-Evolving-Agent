package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/vkviyu/go-self-evolving-agent/internal/render"
)

func (model *Model) View() string {
	if !model.ready {
		return model.styles.metadata.Render("Starting GSEA...")
	}
	if model.width < minimumWidth || model.height < minimumHeight {
		return model.styles.failure.Render("Terminal is too small")
	}
	innerWidth := max(1, model.width-2)
	inputView := model.renderInput()
	parts := []string{
		model.renderHeader(innerWidth),
		model.renderQuestionNavigation(innerWidth),
	}
	if activity := model.renderActivityPanel(innerWidth); activity != "" {
		parts = append(parts, activity)
	}
	parts = append(parts, model.renderViewport(), model.renderStatus(innerWidth), model.renderCommandSuggestions(innerWidth), inputView)
	content := lipgloss.JoinVertical(lipgloss.Left, parts...)
	frame := lipgloss.NewStyle().Width(model.width).Padding(0, 1).Render(content)
	if model.inputSelection.active {
		return frame
	}
	return frame + model.cursorMarker()
}

func (model *Model) resize(width, height int) {
	model.width = width
	model.height = height
	innerWidth := max(1, width-2)
	model.input.SetWidth(innerWidth)
	model.viewport.Width = innerWidth
	model.ready = true
	model.resizeInput()
	model.refreshTranscript(false)
}

func (model *Model) resizeInput() {
	inputHeight := min(max(1, model.inputDisplayHeight()), maximumInputHeight)
	if model.ready {
		// Keep one header, navigation, transcript, status and suggestions row.
		reserved := 5 + max(0, model.suggestionHeight()-1)
		if model.hasActivityOverview() {
			reserved++
		}
		inputHeight = min(inputHeight, max(1, model.height-reserved))
	}
	model.input.SetHeight(inputHeight)
	model.updateInputScroll()
	if !model.ready {
		return
	}
	// Navigation, status and command suggestions each occupy one row.
	model.viewport.Height = max(1, model.height-model.headerHeight()-3-inputHeight-model.activityHeight()-max(0, model.suggestionHeight()-1))
	model.refreshTranscript(false)
}

func (model *Model) inputDisplayHeight() int {
	lines := model.inputDisplayLines()
	return max(1, len(lines))
}

func (model *Model) updateInputScroll() {
	lineCount := len(model.inputDisplayLines())
	height := max(1, model.input.Height())
	if lineCount <= height {
		model.inputScrollOffset = 0
		return
	}

	cursorLine := model.inputCursorDisplayLine()
	maximum := max(0, lineCount-height)
	offset := max(0, min(model.inputScrollOffset, maximum))
	if cursorLine < offset {
		offset = cursorLine
	} else if cursorLine >= offset+height {
		offset = cursorLine - height + 1
	}
	model.inputScrollOffset = max(0, min(offset, maximum))
}

func (model *Model) inputCursorDisplayLine() int {
	displayLines := model.inputDisplayLines()
	if len(displayLines) == 0 {
		return 0
	}
	cursorOffset := model.inputCursorOffset()
	for index, line := range displayLines {
		if cursorOffset < line.end {
			return index
		}
		if cursorOffset == line.end {
			if index+1 < len(displayLines) && displayLines[index+1].start == cursorOffset {
				return index + 1
			}
			return index
		}
	}
	return len(displayLines) - 1
}

func (model *Model) inputCursorOffset() int {
	value := model.input.Value()
	parts := strings.Split(value, "\n")
	logicalLine := max(0, min(model.input.Line(), len(parts)-1))
	base := 0
	for index := range logicalLine {
		base += len([]rune(parts[index])) + 1
	}
	info := model.input.LineInfo()
	return base + info.StartColumn + info.ColumnOffset
}

func (model *Model) inputTopRow() int {
	return model.transcriptTopRow() + model.viewport.Height + 2
}

func compactPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if home, err := os.UserHomeDir(); err == nil {
		if relative, relErr := filepath.Rel(home, path); relErr == nil && filepath.IsLocal(relative) {
			if relative == "." {
				return "~"
			}
			return filepath.Join("~", relative)
		}
	}
	return path
}

func (model *Model) renderStatus(width int) string {
	left := model.styles.ready.Render(render.PlainText(model.stage))
	if model.busy || model.startup != nil {
		left = model.spinner.View() + " " + model.styles.status.Render(render.PlainText(model.progressLabel(time.Now())))
	}
	if model.approvalPending != nil {
		left = model.styles.approval.Render("approval required") + " " + model.styles.metadata.Render("↑↓ select  Enter confirm  Esc reject")
	} else if model.showActivity && model.panelFocused {
		left = model.styles.activityHeader.Render(model.panelHint())
	} else if model.viewer != nil {
		left = model.styles.activityHeader.Render("Agent "+shortID(string(model.viewer.agentID))) + "  " + model.styles.metadata.Render("Esc main")
	}
	right := ""
	if model.contextUsage.Window > 0 {
		right = model.styles.metadata.Render(fmt.Sprintf(
			"ctx input %s / %s  %s · %s",
			formatTokens(model.contextUsage.Used),
			model.contextBudgetLabel(),
			model.contextUsage.Status,
			model.contextUsage.WindowSource,
		))
		if hint := model.contextPendingHint(false); hint != "" {
			right += "  " + model.styles.metadata.Render(hint)
		}
	}
	if model.lastUsage.Total > 0 {
		usageLabel := fmt.Sprintf(
			"in %d  out %d  reasoning %d  total %d",
			model.lastUsage.Input,
			model.lastUsage.Output,
			model.lastUsage.Reasoning,
			model.lastUsage.Total,
		)
		if model.lastUsage.Cached > 0 {
			usageLabel += fmt.Sprintf("  cache %s %.1f%%", formatTokens(model.lastUsage.Cached), model.lastUsage.CacheHitPercent())
		}
		if right == "" {
			right = model.styles.metadata.Render(usageLabel)
		} else {
			right += "  " + model.styles.metadata.Render(usageLabel)
		}
	}
	if right == "" {
		if model.effort != "" {
			right = model.styles.metadata.Render("reasoning " + render.PlainText(model.effort))
		} else {
			right = model.styles.metadata.Render(fmt.Sprintf("%d turns", model.completedTurns))
		}
	}
	if model.clipboardNotice != "" {
		notice := model.styles.metadata.Render(render.PlainText(model.clipboardNotice))
		if right == "" {
			right = notice
		} else {
			right += "  " + notice
		}
	}
	if model.viewer != nil {
		right = model.styles.metadata.Render("main ") + right
	}
	available := width - lipgloss.Width(left) - lipgloss.Width(right)
	if available < 1 {
		compact := model.compactStatusRight(width)
		available = width - lipgloss.Width(compact) - 1
		if available <= 0 {
			return ansi.Truncate(compact, width, "")
		}
		return ansi.Truncate(left, available, "") + " " + compact
	}
	return left + strings.Repeat(" ", available) + right
}

func (model *Model) compactStatusRight(width int) string {
	parts := make([]string, 0, 4)
	if model.contextUsage.Window > 0 {
		parts = append(parts, fmt.Sprintf("ctx input %s/%s",
			formatTokens(model.contextUsage.Used),
			model.contextBudgetLabel()))
	}
	if model.lastUsage.Cached > 0 {
		parts = append(parts, "cache "+formatTokens(model.lastUsage.Cached))
	}
	if hint := model.contextPendingHint(true); hint != "" {
		parts = append(parts, hint)
	}
	if len(parts) == 0 && model.lastUsage.Total > 0 {
		parts = append(parts, fmt.Sprintf("usage %d", model.lastUsage.Total))
	}
	compact := strings.Join(parts, "  ")
	if model.viewer != nil && compact != "" {
		compact = "main " + compact
	}
	if ansi.StringWidth(compact) > width {
		compact = strings.Replace(compact, "ctx input ", "ctx ", 1)
	}
	return model.styles.metadata.Render(compact)
}

func formatTokens(tokens int) string {
	if tokens < 1000 {
		return fmt.Sprintf("%d", tokens)
	}
	if tokens < 10000 {
		return fmt.Sprintf("%.1fk", float64(tokens)/1000)
	}
	return fmt.Sprintf("%dk", tokens/1000)
}

func (model *Model) renderCommandSuggestions(width int) string {
	if len(model.memorySuggestionItems) > 0 {
		rows := make([]string, 0, len(model.memorySuggestionItems))
		for index, item := range model.memorySuggestionItems {
			claim := singleLineMemoryClaim(string(item.Memory.Claim), max(1, width-ansi.StringWidth(item.ID)-12))
			line := fmt.Sprintf("@memory:%s  %s", item.ID, claim)
			if index == model.memorySuggestionIndex {
				rows = append(rows, model.styles.approvalSelected.Render("› "+line))
			} else {
				rows = append(rows, model.styles.suggest.Render("  "+line))
			}
		}
		return lipgloss.JoinVertical(lipgloss.Left, rows...)
	}
	suggestions := append([]string(nil), model.commandSuggestions...)
	for _, value := range model.memorySuggestions {
		suggestions = append(suggestions, "@memory:"+value)
	}
	if len(suggestions) == 0 {
		return strings.Repeat(" ", max(0, width))
	}
	return ansi.Truncate(model.styles.suggest.Render(strings.Join(suggestions, "  ")), width, "...")
}

func (model *Model) suggestionHeight() int {
	if len(model.memorySuggestionItems) > 0 {
		return min(len(model.memorySuggestionItems), 6)
	}
	return 1
}

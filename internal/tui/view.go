package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/vkviyu/go-self-evolving-agent/internal/render"
)

// View renders the visible terminal frame at the current dimensions, including transcript,
// status and input.
func (model *Model) View() tea.View {
	view := tea.View{AltScreen: true, MouseMode: tea.MouseModeCellMotion, WindowTitle: "GSEA · " + model.displaySessionTitle()}
	if !model.ready {
		view.SetContent(model.styles.metadata.Render("Starting GSEA..."))
		return view
	}
	if model.width < minimumWidth || model.height < minimumHeight {
		view.SetContent(model.styles.failure.Render("Terminal is too small"))
		return view
	}
	innerWidth := max(1, model.width-2)
	inputView := model.input.View()
	parts := []string{
		model.renderHeader(innerWidth),
		model.renderQuestionNavigation(innerWidth),
	}
	if activity := model.renderActivityPanel(innerWidth); activity != "" {
		parts = append(parts, activity)
	}
	parts = append(parts, model.renderViewport(), model.renderStatus(innerWidth), model.renderCommandSuggestions(innerWidth), inputView)
	content := lipgloss.JoinVertical(lipgloss.Left, parts...)
	view.SetContent(lipgloss.NewStyle().Width(model.width).Padding(0, 1).Render(content))
	if cursor := model.input.Cursor(); cursor != nil && !model.input.HasSelection() {
		cursor.X++ // The frame has one column of outer padding.
		cursor.Y += model.inputTopRow()
		view.Cursor = cursor
	}
	return view
}

func (model *Model) resize(width, height int) {
	model.width = width
	model.height = height
	innerWidth := max(1, width-2)
	model.viewport.SetWidth(innerWidth)
	model.ready = true
	model.resizeInput()
}

// resizeInput fits the editor to its wrapped lines and recomputes the viewport height from
// the remaining terminal rows.
func (model *Model) resizeInput() {
	if !model.ready {
		return
	}
	// Reflow at the available width before applying the screen height limit.
	model.input.SetWidth(max(1, model.width-2))

	// Keep one header, navigation, transcript, status and suggestions row.
	reserved := 5 + max(0, model.suggestionHeight()-1)
	if model.hasActivityOverview() {
		reserved++
	}
	if available := max(1, model.height-reserved); model.input.Height() > available {
		model.input.SetHeight(available)
	}
	// Navigation, status and command suggestions each occupy one row.
	model.viewport.SetHeight(max(1, model.height-model.headerHeight()-3-model.input.Height()-model.activityHeight()-max(0, model.suggestionHeight()-1)))
	model.refreshTranscript(false)
}

func (model *Model) inputTopRow() int {
	return model.transcriptTopRow() + model.viewport.Height() + 2
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

// formatTokens abbreviates token counts in thousands to fit the terminal status line.
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

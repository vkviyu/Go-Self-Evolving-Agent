package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/vkviyu/go-self-evolving-agent/internal/render"
)

// transcriptSpan describes the final, cell-wrapped display, rather than the
// unwrapped model text. Scroll restoration, disclosure and query navigation all
// use these same coordinates.
type transcriptSpan struct {
	toolEnd    int
	blockIndex int
	start      int
	end        int
}

type queryAnchor struct {
	start   int
	end     int
	preview string
}

type transcriptPosition struct {
	blockIndex int
	within     int
	valid      bool
}

func (model *Model) readingPosition() transcriptPosition {
	var position transcriptPosition
	for _, span := range model.transcriptLayout {
		if span.start > model.viewport.YOffset {
			break
		}
		position = transcriptPosition{blockIndex: span.blockIndex, within: model.viewport.YOffset - span.start, valid: true}
	}
	return position
}

func (model *Model) restoreReadingPosition(position transcriptPosition) {
	if !position.valid {
		return
	}
	nearest := -1
	for index, span := range model.transcriptLayout {
		if span.blockIndex > position.blockIndex {
			break
		}
		nearest = index
	}
	if nearest < 0 {
		return
	}
	span := model.transcriptLayout[nearest]
	end := span.end
	if span.blockIndex == position.blockIndex && nearest+1 < len(model.transcriptLayout) {
		// The viewport can be on the separator below this block. Keep it
		// anchored there when a live refresh leaves the separator intact.
		end = model.transcriptLayout[nearest+1].start
	}
	model.viewport.SetYOffset(span.start + min(position.within, end-span.start-1))
}

// currentQuestion associates the top visible transcript with its preceding
// human query. Background wake messages intentionally do not start a query.
func (model *Model) currentQuestion() (queryAnchor, bool) {
	var current queryAnchor
	found := false
	for _, query := range model.queryAnchors {
		if query.start > model.viewport.YOffset {
			break
		}
		current, found = query, true
	}
	return current, found
}

func (model *Model) renderQuestionNavigation(width int) string {
	if model.toolInspector != nil {
		return model.toolInspectionHeader(width)
	}
	if query, ok := model.currentQuestion(); ok && query.end <= model.viewport.YOffset {
		prefix, hint := "↑ ", "  Alt+P"
		preview := ansi.Truncate(query.preview, max(1, width-ansi.StringWidth(prefix+hint)), "…")
		return model.styles.userMarker.Render(prefix+preview) + model.styles.metadata.Render(hint)
	}
	return model.styles.separator.Render(strings.Repeat("-", max(1, width)))
}

func (model *Model) jumpToQuestion() bool {
	query, ok := model.currentQuestion()
	if !ok {
		return false
	}
	model.autoScroll = false
	model.selection = transcriptSelection{}
	model.viewport.SetYOffset(query.start)
	return true
}

func (model *Model) transcriptTopRow() int {
	return model.headerHeight() + 1 + model.activityHeight()
}

func singleLinePreview(value string, width int) string {
	return ansi.Truncate(strings.Join(strings.Fields(render.PlainText(value)), " "), max(1, width), "…")
}

func (model *Model) displaySessionTitle() string {
	if model.sessionTitle != "" {
		return model.sessionTitle
	}
	return "New conversation"
}

func (model *Model) windowTitleCommand() tea.Cmd {
	return tea.SetWindowTitle("GSEA · " + model.displaySessionTitle())
}

func (model *Model) headerHeight() int {
	if !model.showSessionInfo {
		return 1
	}
	// Preserve room for navigation, status, suggestions and one transcript row.
	reserved := 4
	if model.hasActivityOverview() {
		reserved++
	}
	return min(4, max(1, model.height-model.input.Height()-reserved))
}

func (model *Model) renderHeader(width int) string {
	marker := "▸"
	if model.showSessionInfo {
		marker = "▾"
	}
	prefix, hint := marker+" GSEA  ", "  Alt+I"
	title := ansi.Truncate(model.displaySessionTitle(), max(1, width-ansi.StringWidth(prefix+hint)), "…")
	lines := []string{model.styles.brand.Render(prefix) + model.styles.userMarker.Render(title) + model.styles.metadata.Render(hint)}
	if model.showSessionInfo {
		modelLabel := model.modelName
		if modelLabel == "" {
			modelLabel = "not selected"
		}
		if model.provider != "" {
			modelLabel = model.provider + "/" + modelLabel
		}
		workspace := compactPath(model.workspace)
		if workspace == "" {
			workspace = "not selected"
		}
		for _, detail := range []struct{ label, value string }{{"model      ", modelLabel}, {"workspace  ", workspace}, {"session    ", model.sessionID}} {
			line := detail.label + singleLinePreview(detail.value, width)
			lines = append(lines, model.styles.metadata.Render(ansi.Truncate(line, width, "…")))
		}
	}
	return strings.Join(lines[:min(len(lines), model.headerHeight())], "\n")
}

func (model *Model) toggleSessionInfo() {
	model.showSessionInfo = !model.showSessionInfo
	model.resizeInput()
}

func (model *Model) handleHeaderMouse(message tea.MouseMsg) (tea.Cmd, bool) {
	event := tea.MouseEvent(message)
	if event.Action != tea.MouseActionPress || event.Button != tea.MouseButtonLeft {
		return nil, false
	}
	if event.Y == 0 {
		model.toggleSessionInfo()
		return nil, true
	}
	if event.Y == model.headerHeight() {
		if model.toolInspector != nil {
			model.backFromTool()
			return nil, true
		}
		if query, ok := model.currentQuestion(); ok && query.end <= model.viewport.YOffset {
			model.jumpToQuestion()
			return nil, true
		}
	}
	return nil, false
}

func (model *Model) handleActivityMouse(message tea.MouseMsg) (tea.Cmd, bool) {
	event := tea.MouseEvent(message)
	wheel := event.Button == tea.MouseButtonWheelUp || event.Button == tea.MouseButtonWheelDown
	if !wheel && (event.Action != tea.MouseActionPress || event.Button != tea.MouseButtonLeft) {
		return nil, false
	}
	rowIndex := event.Y - model.headerHeight() - 1
	if rowIndex < 0 || event.X < 1 || event.X >= model.width-1 {
		return nil, false
	}
	rows := model.activityPanelRows(max(1, model.width-2))
	if rowIndex >= len(rows) {
		return nil, false
	}
	if model.approvalPending != nil {
		return nil, true
	}
	if wheel {
		if !model.showActivity || model.panelRows() == 0 {
			return nil, false
		}
		direction := 1
		if event.Button == tea.MouseButtonWheelUp {
			direction = -1
		}
		model.panelFocused = true
		model.panelConfirm = ""
		model.panelSelected += direction
		model.clampPanelSelection()
		return nil, true
	}
	if rowIndex == 0 {
		return model.toggleActivity(), true
	}
	row := rows[rowIndex]
	if !row.selectable {
		return nil, true
	}
	model.panelSelected = row.index
	model.panelConfirm = ""
	if agent, ok := model.selectedAgent(); ok {
		model.panelFocused = false
		return model.openAgentTranscript(agent), true
	}
	model.panelFocused = true
	return nil, true
}

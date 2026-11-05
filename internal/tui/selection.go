package tui

import (
	"strings"

	"github.com/atotto/clipboard"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

type transcriptSelection struct {
	active      bool
	dragging    bool
	start       int
	end         int
	startColumn int
	endColumn   int
}

func (model *Model) handleTranscriptMouse(message tea.MouseMsg) (tea.Cmd, bool) {
	event := tea.MouseEvent(message)
	if event.IsWheel() {
		return nil, false
	}
	line, inside := model.transcriptLine(event.Y)
	switch {
	case event.Action == tea.MouseActionPress && event.Button == tea.MouseButtonLeft:
		if !inside {
			return nil, false
		}
		if model.toggleToolDisclosureAtLine(line) {
			model.refreshTranscript(false)
			return nil, true
		}
		column := max(0, event.X-1)
		model.selection = transcriptSelection{
			active:      true,
			dragging:    true,
			start:       line,
			end:         line,
			startColumn: column,
			endColumn:   column,
		}
		model.clearInputSelection()
		model.autoScroll = false
		model.refreshTranscript(false)
		return nil, true
	case event.Action == tea.MouseActionMotion && model.selection.dragging:
		if inside {
			model.selection.end = line
			model.selection.endColumn = max(0, event.X-1)
		}
		model.refreshTranscript(false)
		return nil, true
	case event.Action == tea.MouseActionRelease && model.selection.dragging:
		if inside {
			model.selection.end = line
			model.selection.endColumn = max(0, event.X-1)
		}
		model.selection.dragging = false
		text, lines := model.selectedTranscript()
		model.refreshTranscript(false)
		if text == "" {
			return nil, true
		}
		return copyToClipboard(text, lines), true
	default:
		return nil, false
	}
}

func (model *Model) handleInputMouse(message tea.MouseMsg) (tea.Cmd, bool) {
	event := tea.MouseEvent(message)
	if event.IsWheel() {
		return nil, false
	}
	inside := event.Y >= model.inputTopRow() && event.Y < model.inputTopRow()+model.input.Height()
	if !inside && !model.inputSelection.dragging {
		return nil, false
	}
	switch {
	case event.Action == tea.MouseActionPress && event.Button == tea.MouseButtonLeft && inside:
		model.input.Focus()
		model.selection = transcriptSelection{}
		offset := model.inputOffsetAt(event.Y, event.X)
		model.inputSelection = inputTextSelection{active: true, dragging: true, start: offset, end: offset}
		return nil, true
	case event.Action == tea.MouseActionMotion && model.inputSelection.dragging:
		if inside {
			model.inputSelection.end = model.inputOffsetAt(event.Y, event.X)
		}
		return nil, true
	case event.Action == tea.MouseActionRelease && model.inputSelection.dragging:
		if inside {
			model.inputSelection.end = model.inputOffsetAt(event.Y, event.X)
		}
		model.inputSelection.dragging = false
		if selected := model.selectedInputText(); selected != "" {
			return copyToClipboard(selected, strings.Count(selected, "\n")+1), true
		}
		model.clearInputSelection()
		return nil, true
	default:
		return nil, false
	}
}

func (model *Model) transcriptLine(row int) (int, bool) {
	top := model.transcriptTopRow()
	if row < top || row >= top+model.viewport.Height {
		return 0, false
	}
	line := model.viewport.YOffset + row - top
	contentLines := strings.Split(model.transcriptContent, "\n")
	if line < 0 || line >= len(contentLines) {
		return 0, false
	}
	return line, true
}

func (model *Model) selectedTranscript() (string, int) {
	lines := strings.Split(model.transcriptContent, "\n")
	if len(lines) == 0 {
		return "", 0
	}
	start, end, startColumn, endColumn := model.selectionBounds()
	start = max(0, min(start, len(lines)-1))
	end = max(0, min(end, len(lines)-1))
	selected := make([]string, 0, end-start+1)
	for index, line := range lines[start : end+1] {
		left, right := 0, ansi.StringWidth(line)
		if index == 0 {
			left = startColumn
		}
		if index == end-start {
			right = endColumn
		}
		left = max(0, min(left, right))
		right = max(left, min(right, ansi.StringWidth(line)))
		selected = append(selected, ansi.Strip(ansi.Cut(line, left, right)))
	}
	return strings.Join(selected, "\n"), len(selected)
}

func copyToClipboard(text string, lines int) tea.Cmd {
	return func() tea.Msg {
		return clipboardResultMsg{lines: lines, err: clipboard.WriteAll(text)}
	}
}

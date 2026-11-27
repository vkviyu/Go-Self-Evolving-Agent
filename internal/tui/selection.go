package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/atotto/clipboard"
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
	if _, wheel := message.(tea.MouseWheelMsg); wheel {
		return nil, false
	}
	event := message.Mouse()
	line, inside := model.transcriptLine(event.Y)
	switch message.(type) {
	case tea.MouseClickMsg:
		if event.Button != tea.MouseLeft || !inside {
			return nil, false
		}
		if model.toggleToolDisclosureAtLine(line) {
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
		model.input.ClearSelection()
		model.autoScroll = false
		model.refreshTranscript(false)
		return nil, true
	case tea.MouseMotionMsg:
		if !model.selection.dragging {
			return nil, false
		}
		if inside {
			model.selection.end = line
			model.selection.endColumn = max(0, event.X-1)
		}
		model.refreshTranscript(false)
		return nil, true
	case tea.MouseReleaseMsg:
		if !model.selection.dragging {
			return nil, false
		}
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
	event := message.Mouse()
	inside := event.Y >= model.inputTopRow() && event.Y < model.inputTopRow()+model.input.Height()
	x, y := event.X-1, event.Y-model.inputTopRow()
	switch message.(type) {
	case tea.MouseClickMsg:
		if !inside || event.Button != tea.MouseLeft {
			return nil, false
		}
		model.closeToolInspector()
		model.input.Focus()
		model.selection = transcriptSelection{}
		model.draggingInput = true
		model.input.BeginSelection(x, y)
		return nil, true
	case tea.MouseMotionMsg, tea.MouseReleaseMsg:
		if !model.draggingInput {
			return nil, false
		}
		if inside {
			model.input.ExtendSelection(x, y)
		}
		if _, released := message.(tea.MouseReleaseMsg); released {
			model.draggingInput = false
			model.input.EndSelection()
			if text := model.input.SelectedText(); text != "" {
				return copyToClipboard(text, strings.Count(text, "\n")+1), true
			}
		}
		return nil, true
	default:
		return nil, false
	}
}

func (model *Model) transcriptLine(row int) (int, bool) {
	top := model.transcriptTopRow()
	if row < top || row >= top+model.viewport.Height() {
		return 0, false
	}
	line := model.viewport.YOffset() + row - top
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

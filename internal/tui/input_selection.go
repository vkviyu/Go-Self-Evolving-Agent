package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

type inputTextSelection struct {
	active   bool
	dragging bool
	start    int
	end      int
}

type inputDisplayLine struct {
	start int
	end   int
}

func (model *Model) clearInputSelection() {
	model.inputSelection = inputTextSelection{}
}

func (model *Model) selectAllInput() {
	length := len([]rune(model.input.Value()))
	model.selection = transcriptSelection{}
	model.inputSelection = inputTextSelection{
		active: length > 0,
		start:  0,
		end:    length,
	}
}

func (model *Model) selectedInputText() string {
	if !model.inputSelection.active {
		return ""
	}
	start, end := model.inputSelectionBounds()
	runes := []rune(model.input.Value())
	start = max(0, min(start, len(runes)))
	end = max(start, min(end, len(runes)))
	return string(runes[start:end])
}

func (model *Model) inputSelectionBounds() (int, int) {
	start, end := model.inputSelection.start, model.inputSelection.end
	if start > end {
		start, end = end, start
	}
	return start, end
}

func (model *Model) inputDisplayLines() []inputDisplayLine {
	value := model.input.Value()
	width := model.input.Width()
	if model.inputLayoutReady && model.inputLayoutValue == value && model.inputLayoutWidth == width {
		return model.inputLayout
	}
	parts := strings.Split(value, "\n")
	lines := make([]inputDisplayLine, 0, len(parts))
	offset := 0
	for index, part := range parts {
		for _, span := range model.wrapInputLine(part) {
			lines = append(lines, inputDisplayLine{start: offset + span.start, end: offset + span.end})
		}
		offset += len([]rune(part))
		if index < len(parts)-1 {
			offset++
		}
	}
	if len(lines) == 0 {
		lines = []inputDisplayLine{{}}
	}
	model.inputLayoutValue = value
	model.inputLayoutWidth = width
	model.inputLayout = lines
	model.inputLayoutReady = true
	return lines
}

// wrapInputLine computes source spans in one word-wrap pass. Probing the
// widget at every cursor position repeatedly scanned its complete row map.
func (model *Model) wrapInputLine(source string) []inputDisplayLine {
	return wrapInputSpans(source, model.input.Width())
}

func (model *Model) inputVisibleStart(lineCount int) int {
	height := max(1, model.input.Height())
	return max(0, min(model.inputScrollOffset, max(0, lineCount-height)))
}

func (model *Model) inputOffsetAt(row, column int) int {
	lines := model.inputDisplayLines()
	if len(lines) == 0 {
		return 0
	}
	lineIndex := model.inputVisibleStart(len(lines)) + max(0, row-model.inputTopRow())
	lineIndex = max(0, min(lineIndex, len(lines)-1))
	line := lines[lineIndex]
	column = max(0, column-1-ansi.StringWidth(model.input.Prompt))
	runes := []rune(model.input.Value())
	width := 0
	for index := line.start; index < line.end; index++ {
		characterWidth := ansi.StringWidth(string(runes[index]))
		if column < width+max(1, characterWidth) {
			return index
		}
		width += characterWidth
	}
	return line.end
}

func (model *Model) renderInput() string {
	if model.input.Value() == "" {
		prompt := model.input.FocusedStyle.Prompt.Render(model.input.Prompt)
		placeholder := model.input.FocusedStyle.Placeholder.Render(model.input.Placeholder)
		placeholderWidth := ansi.StringWidth(placeholder)
		placeholder += strings.Repeat(" ", max(0, model.input.Width()-placeholderWidth))
		return prompt + placeholder
	}
	displayLines := model.inputDisplayLines()
	visibleStart := model.inputVisibleStart(len(displayLines))
	visibleEnd := min(len(displayLines), visibleStart+max(1, model.input.Height()))
	if len(displayLines) == 0 {
		return ""
	}
	runes := []rune(model.input.Value())
	start, end := model.inputSelectionBounds()
	selected := model.inputSelection.active && start != end
	lines := make([]string, 0, visibleEnd-visibleStart)
	for displayIndex := visibleStart; displayIndex < visibleEnd; displayIndex++ {
		line := displayLines[displayIndex]
		text := string(runes[line.start:line.end])
		text = ansi.Cut(text, 0, model.input.Width())
		textWidth := ansi.StringWidth(text)
		text += strings.Repeat(" ", max(0, model.input.Width()-textWidth))
		if selected {
			left := ansi.StringWidth(string(runes[line.start:max(line.start, min(start, line.end))]))
			right := ansi.StringWidth(string(runes[line.start:max(line.start, min(end, line.end))]))
			text = highlightSelection(text, left, right, model.styles.selection)
		}
		if !selected && displayIndex == model.inputCursorDisplayLine() {
			text = model.renderInputCursor(text, line)
		}
		prompt := model.input.FocusedStyle.Prompt.Render(model.input.Prompt)
		lines = append(lines, prompt+text)
	}
	return strings.Join(lines, "\n")
}

func (model *Model) renderInputCursor(text string, line inputDisplayLine) string {
	cursorOffset := model.inputCursorOffset()
	relative := max(0, min(cursorOffset-line.start, line.end-line.start))
	lineRunes := []rune(text)
	if relative < len(lineRunes) {
		model.input.Cursor.TextStyle = model.input.FocusedStyle.CursorLine.Inherit(model.input.FocusedStyle.Base).Inline(true)
		model.input.Cursor.SetChar(string(lineRunes[relative]))
		cursor := model.input.Cursor.View()
		before := string(lineRunes[:relative])
		after := string(lineRunes[relative+1:])
		return before + cursor + after
	}
	model.input.Cursor.TextStyle = model.input.FocusedStyle.CursorLine.Inherit(model.input.FocusedStyle.Base).Inline(true)
	model.input.Cursor.SetChar(" ")
	return text + model.input.Cursor.View()
}

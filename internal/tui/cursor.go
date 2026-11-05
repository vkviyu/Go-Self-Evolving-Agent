package tui

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/term"
)

// cursorMarker is consumed by cursorWriter after Bubble Tea has rendered a
// frame. The standard renderer leaves the terminal cursor at column zero;
// keeping the physical cursor beside the input text is required by native
// IMEs, which draw uncommitted text at the terminal cursor position.
const cursorMarker = "\x1b]9;gsea-cursor:"

func (model *Model) cursorMarker() string {
	if !model.ready {
		return ""
	}
	line := model.input.LineInfo()
	cursorRow := model.inputCursorDisplayLine() - model.inputVisibleStart(model.inputDisplayHeight())
	targetRow := model.inputTopRow() + cursorRow + 1
	linesUp := max(0, model.height-targetRow)
	// The frame has one column of outer padding and the two-column prompt.
	return cursorMarker + strconv.Itoa(linesUp) + "," + strconv.Itoa(1+2+line.CharOffset) + "\a"
}

type cursorWriter struct {
	output io.Writer
}

type terminalCursorWriter struct {
	cursorWriter
	term.File
}

func (writer terminalCursorWriter) Write(data []byte) (int, error) {
	return writer.cursorWriter.Write(data)
}

func wrapCursorWriter(output io.Writer) io.Writer {
	writer := cursorWriter{output: output}
	file, ok := output.(term.File)
	if !ok {
		return writer
	}
	return terminalCursorWriter{cursorWriter: writer, File: file}
}

func (writer cursorWriter) Write(data []byte) (int, error) {
	markerStart := bytes.Index(data, []byte(cursorMarker))
	if markerStart < 0 {
		return writer.output.Write(data)
	}
	valueStart := markerStart + len(cursorMarker)
	valueEnd := bytes.IndexByte(data[valueStart:], '\a')
	if valueEnd < 0 {
		return writer.output.Write(data)
	}
	valueEnd += valueStart
	linesUp, column, ok := parseCursorPosition(string(data[valueStart:valueEnd]))
	if !ok {
		return writer.output.Write(data)
	}
	cleaned := make([]byte, 0, len(data)+16)
	cleaned = append(cleaned, data[:markerStart]...)
	cleaned = append(cleaned, data[valueEnd+1:]...)
	if row, ok := finalCursorRow(cleaned); ok {
		position := fmt.Appendf(nil, "\x1b[%d;%dH", max(1, row-linesUp), column+1)
		cleaned = append(cleaned[:bytes.LastIndex(cleaned, []byte("\x1b["))], position...)
	} else {
		// Bubble Tea positions the next inline frame vertically while preserving
		// the current column. Reset first because the previous frame ends at the
		// input cursor rather than column zero.
		cleaned = append([]byte{'\r'}, cleaned...)
		lastReturn := bytes.LastIndexByte(cleaned, '\r')
		if lastReturn < 0 {
			return writer.output.Write(data)
		}
		position := fmt.Appendf(nil, "\x1b[%dG", column+1)
		if linesUp > 0 {
			position = fmt.Appendf(nil, "\x1b[%dA%s", linesUp, position)
		}
		cleaned = append(cleaned[:lastReturn], append(position, cleaned[lastReturn+1:]...)...)
	}
	if _, err := writer.output.Write(cleaned); err != nil {
		return len(data), err
	}
	return len(data), nil
}

func parseCursorPosition(value string) (linesUp, column int, ok bool) {
	parts := strings.Split(value, ",")
	if len(parts) == 1 {
		column, err := strconv.Atoi(parts[0])
		return 0, column, err == nil && column >= 0
	}
	if len(parts) != 2 {
		return 0, 0, false
	}
	linesUp, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	column, err = strconv.Atoi(parts[1])
	return linesUp, column, err == nil && linesUp >= 0 && column >= 0
}

func finalCursorRow(data []byte) (int, bool) {
	if !bytes.HasSuffix(data, []byte(";H")) {
		return 0, false
	}
	start := bytes.LastIndex(data, []byte("\x1b["))
	if start < 0 {
		return 0, false
	}
	row, err := strconv.Atoi(string(data[start+2 : len(data)-2]))
	return row, err == nil && row > 0
}

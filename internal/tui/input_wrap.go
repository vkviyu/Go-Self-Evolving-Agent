package tui

import (
	"unicode"

	"github.com/mattn/go-runewidth"
	"github.com/rivo/uniseg"
)

// The wrapping decisions mirror bubbles/textarea v1.0.0's MIT-licensed wrap
// algorithm (Copyright Charmbracelet, Inc.), including its trailing cursor
// space. We retain source offsets instead of probing LineInfo for every rune.
func wrapInputSpans(source string, width int) []inputDisplayLine {
	runes := []rune(source)
	width = max(1, width)
	spans := make([]inputDisplayLine, 0, len(runes)/width+1)
	var line, word []rune
	lineWidth, wordWidth, spaces := 0, 0, 0
	start, assigned := 0, 0
	flushLine := func() {
		end := min(assigned, len(runes))
		spans = append(spans, inputDisplayLine{start: start, end: end})
		start = end
		line = line[:0]
		lineWidth = 0
	}
	appendWord := func() {
		line = append(line, word...)
		assigned += len(word) + spaces
		for range spaces {
			line = append(line, ' ')
		}
		lineWidth = uniseg.StringWidth(string(line))
		word = word[:0]
		wordWidth, spaces = 0, 0
	}
	for _, value := range runes {
		if unicode.IsSpace(value) {
			spaces++
		} else {
			word = append(word, value)
			if value >= ' ' && value <= '~' {
				wordWidth++
			} else {
				// Grapheme width is not additive for ZWJ/variation selectors.
				wordWidth = uniseg.StringWidth(string(word))
			}
		}
		if spaces > 0 {
			if lineWidth+wordWidth+spaces > width {
				flushLine()
			}
			appendWord()
		} else if wordWidth+runewidth.RuneWidth(value) > width {
			if len(line) > 0 {
				flushLine()
			}
			appendWord()
		}
	}
	if lineWidth+wordWidth+spaces >= width {
		flushLine()
	}
	spaces++ // textarea reserves one trailing cursor cell
	appendWord()
	flushLine()
	return spans
}

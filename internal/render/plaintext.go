package render

import (
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

// TextSanitizer discards terminal commands from untrusted text. Parser state
// survives stream chunks, so splitting an OSC/CSI or UTF-8 rune cannot bypass
// the boundary. Sequence payloads use bounded scratch space, not their length.
type TextSanitizer struct {
	parser *ansi.Parser
	output strings.Builder
}

// Filter returns printable text from this fragment, preserving parser state across partial
// UTF-8 and terminal escape sequences.
func (sanitizer *TextSanitizer) Filter(text string) string {
	if sanitizer.parser == nil {
		sanitizer.parser = new(ansi.Parser)
		sanitizer.parser.SetParamsSize(32)
		sanitizer.parser.SetDataSize(1)
		sanitizer.parser.SetHandler(ansi.Handler{
			Print: func(value rune) {
				if !unicode.IsControl(value) {
					sanitizer.output.WriteRune(value)
				}
			},
			Execute: func(value byte) {
				if value == '\n' || value == '\t' {
					sanitizer.output.WriteByte(value)
				}
			},
		})
	}
	sanitizer.output.Reset()
	for index := range len(text) {
		sanitizer.parser.Advance(text[index])
	}
	return sanitizer.output.String()
}

// PlainText is for complete untrusted strings. Styling and host-owned links
// are added only after this boundary; durable model/tool data is unchanged.
func PlainText(text string) string {
	var sanitizer TextSanitizer
	return sanitizer.Filter(text)
}

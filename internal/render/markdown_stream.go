package render

import (
	"io"
	"strings"

	"github.com/alecthomas/chroma/v2/quick"
)

const (
	ansiReset    = "\x1b[0m"
	ansiMetadata = "\x1b[38;5;243m"
)

func writeStyled(writer io.Writer, value, color string, enabled bool) error {
	value = PlainText(value)
	if enabled {
		value = color + value + ansiReset
	}
	_, err := io.WriteString(writer, value)
	return err
}

// markdownStreamRenderer keeps prose and complete code lines streaming while
// retaining only the current incomplete line needed to identify Markdown fences.
type markdownStreamRenderer struct {
	writer        io.Writer
	color         bool
	pending       string
	inFence       bool
	codeLineStart bool
	fence         byte
	fenceLen      int
	language      string
	sanitizer     TextSanitizer
	proseLineOpen bool
}

const maxMarkdownPendingBytes = 4096

// MarkdownStreamRenderer incrementally renders Markdown while preserving
// immediate output for prose and complete code lines.
type MarkdownStreamRenderer = markdownStreamRenderer

// MetadataColor is the ANSI color used for metadata and usage lines.
const MetadataColor = ansiMetadata

// NewMarkdownStreamRenderer creates a renderer for a streaming assistant
// response. Colorized output is enabled only when the destination is a TTY.
func NewMarkdownStreamRenderer(writer io.Writer, color bool) *MarkdownStreamRenderer {
	return newMarkdownStreamRenderer(writer, color)
}

// WriteStyled writes a value with an optional ANSI color wrapper.
func WriteStyled(writer io.Writer, value, color string, enabled bool) error {
	return writeStyled(writer, value, color, enabled)
}

func newMarkdownStreamRenderer(writer io.Writer, color bool) *markdownStreamRenderer {
	return &markdownStreamRenderer{writer: writer, color: color}
}

// Write renders another untrusted text fragment, retaining parser state across split
// fences and escape sequences.
func (r *markdownStreamRenderer) Write(delta string) error {
	delta = r.sanitizer.Filter(delta)
	if delta == "" {
		return nil
	}
	if !r.color {
		_, err := io.WriteString(r.writer, delta)
		return err
	}
	if r.proseLineOpen {
		newline := strings.IndexByte(delta, '\n')
		if newline < 0 {
			_, err := io.WriteString(r.writer, delta)
			return err
		}
		if _, err := io.WriteString(r.writer, delta[:newline+1]); err != nil {
			return err
		}
		r.proseLineOpen = false
		delta = delta[newline+1:]
	}
	r.pending += delta
	for {
		if r.inFence {
			if err := r.writeCodePending(); err != nil {
				return err
			}
			if r.inFence {
				return nil
			}
			continue
		}
		newline := strings.IndexByte(r.pending, '\n')
		if newline >= 0 {
			line := r.pending[:newline+1]
			r.pending = r.pending[newline+1:]
			if err := r.processLine(line); err != nil {
				return err
			}
			continue
		}
		if potentialFencePrefix(r.pending) {
			if len(r.pending) > maxMarkdownPendingBytes {
				if _, err := io.WriteString(r.writer, r.pending); err != nil {
					return err
				}
				r.pending = ""
				r.proseLineOpen = true
			}
			return nil
		}
		if _, err := io.WriteString(r.writer, r.pending); err != nil {
			return err
		}
		r.pending = ""
		return nil
	}
}

func potentialFencePrefix(value string) bool {
	trimmed := strings.TrimLeft(value, " \t")
	if trimmed == "" {
		return true
	}
	// An inline opening such as "text```go" must stay buffered until the
	// newline arrives so the line-level parser can normalize it into a block.
	if strings.Contains(value, "```") || strings.Contains(value, "~~~") {
		return true
	}
	if len(trimmed) < 3 {
		return strings.HasPrefix("```", trimmed) || strings.HasPrefix("~~~", trimmed)
	}
	return trimmed[0] == '`' || trimmed[0] == '~'
}

// Finish flushes buffered text at the end of the Markdown stream.
func (r *markdownStreamRenderer) Finish() error {
	if !r.color {
		return nil
	}
	if r.inFence {
		if r.pending != "" {
			if _, err := io.WriteString(r.writer, highlightSource(r.pending, r.language)); err != nil {
				return err
			}
			r.pending = ""
		}
		r.inFence = false
		r.codeLineStart = false
		r.fence = 0
		r.fenceLen = 0
		r.language = ""
		return nil
	}
	if r.pending != "" {
		if err := r.processLine(r.pending); err != nil {
			return err
		}
		r.pending = ""
	}
	return nil
}

func (r *markdownStreamRenderer) processLine(line string) error {
	trimmed := strings.TrimSpace(strings.TrimSuffix(line, "\n"))
	if !r.inFence {
		marker, language, ok := parseFenceOpening(trimmed)
		if ok {
			r.inFence = true
			r.codeLineStart = true
			r.fence = marker
			r.fenceLen = fenceLength(trimmed, marker)
			r.language = language
			_, err := io.WriteString(r.writer, line)
			return err
		}
		if prefix, opening, nextMarker, nextLanguage, inlineOK := parseInlineFence(line); inlineOK {
			if _, err := io.WriteString(r.writer, prefix+"\n"+opening); err != nil {
				return err
			}
			r.inFence = true
			r.codeLineStart = true
			r.fence = nextMarker
			r.fenceLen = fenceLength(strings.TrimSpace(strings.TrimSuffix(opening, "\n")), nextMarker)
			r.language = nextLanguage
			return nil
		}
		_, err := io.WriteString(r.writer, line)
		return err
	}

	return nil
}

func parseInlineFence(line string) (string, string, byte, string, bool) {
	lineBody := strings.TrimSuffix(line, "\n")
	for _, candidate := range []byte{'`', '~'} {
		markerText := strings.Repeat(string(candidate), 3)
		index := strings.Index(lineBody, markerText)
		if index <= 0 {
			continue
		}
		opening := lineBody[index:]
		marker, language, ok := parseFenceOpening(strings.TrimSpace(opening))
		if ok {
			if strings.HasSuffix(line, "\n") {
				opening += "\n"
			}
			return lineBody[:index], opening, marker, language, true
		}
	}
	return "", "", 0, "", false
}

func (r *markdownStreamRenderer) writeCodePending() error {
	for {
		newline := strings.IndexByte(r.pending, '\n')
		if newline >= 0 {
			line := r.pending[:newline+1]
			r.pending = r.pending[newline+1:]
			trimmed := strings.TrimSpace(strings.TrimSuffix(line, "\n"))
			if r.codeLineStart && isFenceClosing(trimmed, r.fence, r.fenceLen) {
				if _, err := io.WriteString(r.writer, line); err != nil {
					return err
				}
				r.inFence = false
				r.codeLineStart = false
				r.fence = 0
				r.fenceLen = 0
				r.language = ""
				return nil
			}
			if _, err := io.WriteString(r.writer, highlightSource(line, r.language)); err != nil {
				return err
			}
			r.codeLineStart = true
			continue
		}
		if r.codeLineStart && potentialClosingFencePrefix(r.pending, r.fence, r.fenceLen) {
			if len(r.pending) <= maxMarkdownPendingBytes {
				return nil
			}
		}
		if r.pending == "" {
			return nil
		}
		if _, err := io.WriteString(r.writer, highlightSource(r.pending, r.language)); err != nil {
			return err
		}
		r.pending = ""
		r.codeLineStart = false
		return nil
	}
}

func potentialClosingFencePrefix(value string, marker byte, openingLength int) bool {
	trimmed := strings.TrimLeft(value, " \t")
	if trimmed == "" {
		return true
	}
	if trimmed[0] != marker {
		return false
	}
	length := fenceLength(trimmed, marker)
	if length == len(trimmed) {
		return true
	}
	if length < openingLength {
		return false
	}
	return strings.TrimSpace(trimmed[length:]) == ""
}

func parseFenceOpening(line string) (byte, string, bool) {
	if len(line) < 3 || (line[0] != '`' && line[0] != '~') {
		return 0, "", false
	}
	marker := line[0]
	length := fenceLength(line, marker)
	if length < 3 {
		return 0, "", false
	}
	rest := strings.TrimSpace(line[length:])
	if len(rest) > 128 {
		return 0, "", false
	}
	if strings.ContainsAny(rest, "`~") {
		return 0, "", false
	}
	if space := strings.IndexAny(rest, " \t"); space >= 0 {
		rest = rest[:space]
	}
	return marker, strings.ToLower(strings.TrimSpace(rest)), true
}

func isFenceClosing(line string, marker byte, openingLength int) bool {
	if len(line) < openingLength || line[0] != marker {
		return false
	}
	length := fenceLength(line, marker)
	return length >= openingLength && strings.TrimSpace(line[length:]) == ""
}

// fenceLength counts the leading run of the specified Markdown fence character.
func fenceLength(line string, marker byte) int {
	length := 0
	for length < len(line) && line[length] == marker {
		length++
	}
	return length
}

func highlightSource(source, language string) string {
	var result strings.Builder
	if err := quick.Highlight(&result, source, NormalizeCodeLanguage(language), "terminal256", "monokai"); err != nil {
		return source
	}
	return result.String()
}

// NormalizeCodeLanguage maps common fence aliases (golang, js, py, yml, ...)
// to the language names Chroma recognizes.
func NormalizeCodeLanguage(language string) string {
	language = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(language), "."))
	switch language {
	case "golang":
		return "go"
	case "js", "jsx":
		return "javascript"
	case "ts", "tsx":
		return "typescript"
	case "py":
		return "python"
	case "rs":
		return "rust"
	case "php3", "php4", "php5", "php7", "php8":
		return "php"
	case "yml":
		return "yaml"
	case "sh", "zsh":
		return "shell"
	default:
		return language
	}
}

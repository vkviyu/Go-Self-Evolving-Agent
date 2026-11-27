package tui

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/alecthomas/chroma/v2/quick"
	"github.com/charmbracelet/glamour"
	glamouransi "github.com/charmbracelet/glamour/ansi"
	glamourstyles "github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/x/ansi"

	"github.com/vkviyu/go-self-evolving-agent/internal/render"
)

type markdownRenderer struct {
	width    int
	renderer *glamour.TermRenderer
}

type markdownSegment struct {
	code     bool
	language string
	content  string
}

const (
	minimumMarkdownWidth = 20
	codeBackground       = "\x1b[48;5;235m"
	codeTabWidth         = 4
)

func (renderer *markdownRenderer) render(markdown string, width int) string {
	if strings.TrimSpace(markdown) == "" {
		return ""
	}
	width = max(minimumMarkdownWidth, width)
	if renderer.renderer == nil || renderer.width != width {
		termRenderer, err := glamour.NewTermRenderer(
			glamour.WithStyles(transcriptMarkdownStyle()),
			glamour.WithWordWrap(width),
			glamour.WithPreservedNewLines(),
		)
		if err != nil {
			return ansi.Hardwrap(markdown, width, false)
		}
		renderer.width = width
		renderer.renderer = termRenderer
	}
	segments := splitMarkdownSegments(markdown)
	rendered := make([]string, 0, len(segments))
	for _, segment := range segments {
		if segment.code {
			rendered = append(rendered, renderCodeBlock(segment.content, segment.language, width))
			continue
		}
		if strings.TrimSpace(segment.content) == "" {
			continue
		}
		output, err := renderer.renderer.Render(segment.content)
		if err != nil {
			rendered = append(rendered, ansi.Hardwrap(segment.content, width, false))
			continue
		}
		rendered = append(rendered, strings.TrimSpace(output))
	}
	return strings.TrimSpace(strings.Join(rendered, "\n\n"))
}

func transcriptMarkdownStyle() glamouransi.StyleConfig {
	style := glamourstyles.DarkStyleConfig
	// The transcript supplies its own gutter for both prose and code.
	style.Document.Margin = new(uint)
	style.Document.BlockPrefix = ""
	style.Document.BlockSuffix = ""
	style.Document.Color = nil
	style.H1 = glamouransi.StyleBlock{StylePrimitive: glamouransi.StylePrimitive{Color: new("81")}}
	for _, heading := range []*glamouransi.StyleBlock{&style.H2, &style.H3, &style.H4, &style.H5, &style.H6} {
		heading.Prefix = ""
	}
	style.H3.Color = new("75")
	return style
}

func splitMarkdownSegments(markdown string) []markdownSegment {
	markdown = strings.ReplaceAll(markdown, "\r\n", "\n")
	lines := strings.SplitAfter(markdown, "\n")
	segments := make([]markdownSegment, 0, 3)
	var prose strings.Builder
	var code strings.Builder
	var marker byte
	var markerLength int
	var language string
	inCode := false
	flushProse := func() {
		if prose.Len() == 0 {
			return
		}
		segments = append(segments, markdownSegment{content: prose.String()})
		prose.Reset()
	}
	flushCode := func() {
		segments = append(segments, markdownSegment{code: true, language: language, content: strings.TrimSuffix(code.String(), "\n")})
		code.Reset()
		language = ""
		marker = 0
		markerLength = 0
	}

	for _, rawLine := range lines {
		line := strings.TrimSuffix(rawLine, "\n")
		if !inCode {
			if prefix, nextMarker, nextLength, nextLanguage, ok := markdownFenceOpening(line); ok {
				if strings.TrimSpace(prefix) != "" {
					prose.WriteString(prefix)
					prose.WriteByte('\n')
				}
				flushProse()
				inCode = true
				marker = nextMarker
				markerLength = nextLength
				language = nextLanguage
				continue
			}
			prose.WriteString(rawLine)
			continue
		}
		if isMarkdownFenceClose(line, marker, markerLength) {
			flushCode()
			inCode = false
			continue
		}
		code.WriteString(rawLine)
	}
	if inCode {
		flushCode()
	} else {
		flushProse()
	}
	return segments
}

func markdownFenceOpening(line string) (string, byte, int, string, bool) {
	if marker, length, language, ok := parseMarkdownFence(line); ok {
		return "", marker, length, language, true
	}
	for _, candidate := range []byte{'`', '~'} {
		markerText := strings.Repeat(string(candidate), 3)
		index := strings.Index(line, markerText)
		if index <= 0 {
			continue
		}
		marker, length, language, ok := parseMarkdownFence(line[index:])
		if ok {
			return line[:index], marker, length, language, true
		}
	}
	return "", 0, 0, "", false
}

func parseMarkdownFence(line string) (byte, int, string, bool) {
	line = strings.TrimSpace(line)
	if len(line) < 3 || (line[0] != '`' && line[0] != '~') {
		return 0, 0, "", false
	}
	marker := line[0]
	length := 0
	for length < len(line) && line[length] == marker {
		length++
	}
	if length < 3 {
		return 0, 0, "", false
	}
	rest := strings.TrimSpace(line[length:])
	if strings.ContainsAny(rest, "`~") {
		return 0, 0, "", false
	}
	if fields := strings.Fields(rest); len(fields) > 0 {
		return marker, length, render.NormalizeCodeLanguage(fields[0]), true
	}
	return marker, length, "", true
}

func isMarkdownFenceClose(line string, marker byte, openingLength int) bool {
	line = strings.TrimSpace(line)
	if len(line) < openingLength || line[0] != marker {
		return false
	}
	length := 0
	for length < len(line) && line[length] == marker {
		length++
	}
	return length >= openingLength && strings.TrimSpace(line[length:]) == ""
}

func renderCodeBlock(source, language string, width int) string {
	innerWidth := max(1, width-2)
	source = wrapCode(source, innerWidth)
	var highlighted strings.Builder
	// Provide one terminator to the lexer, then remove only that one so
	// leading, internal and trailing blank lines retain the panel background.
	if err := quick.Highlight(&highlighted, source+"\n", language, "terminal256", "monokai"); err != nil {
		return codePanelRows(source, innerWidth)
	}
	return codePanelRows(strings.TrimSuffix(highlighted.String(), "\n"), innerWidth)
}

func wrapCode(source string, innerWidth int) string {
	return ansi.Hardwrap(strings.ReplaceAll(source, "\t", strings.Repeat(" ", codeTabWidth)), innerWidth, true)
}

func codePanelRows(wrapped string, innerWidth int) string {
	lines := strings.Split(wrapped, "\n")
	for i, line := range lines {
		lines[i] = renderCodeLine(line, innerWidth)
	}
	return strings.Join(lines, "\n")
}

func renderCodeLine(line string, width int) string {
	line = ansi.Cut(line, 0, width)
	lineWidth := ansi.StringWidth(line)
	line = " " + line + strings.Repeat(" ", max(0, width-lineWidth)) + " "
	line = strings.ReplaceAll(line, "\x1b[0m", "\x1b[0m"+codeBackground)
	return codeBackground + line + "\x1b[0m"
}

func formatJSON(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	var formatted bytes.Buffer
	if err := json.Indent(&formatted, []byte(value), "", "  "); err == nil {
		return formatted.String()
	}
	return value
}

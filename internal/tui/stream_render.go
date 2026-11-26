package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/vkviyu/go-self-evolving-agent/internal/render"
)

// A block can move when the transcript slice grows. Keep the append-only
// builders behind a pointer so neither their data nor their state is copied.
type textStream struct {
	raw     strings.Builder
	preview *markdownPreview
}

func (block *transcriptBlock) appendDelta(delta string) {
	if block.stream == nil {
		block.stream = new(textStream)
	}
	block.stream.raw.WriteString(delta)
	block.content = block.stream.raw.String()
}

func (block *transcriptBlock) finishStream() {
	block.stream = nil
	block.pending = false
}

// Paint unfinished text immediately. Once a paragraph or a fenced block
// closes, typeset it once at the current width. The final assistant message
// goes through the normal whole-document renderer, which also resolves
// constructs spanning paragraphs (e.g. reference links and loose lists).
type markdownPreview struct {
	width       int
	consumed    int
	sanitizer   render.TextSanitizer
	plain       strings.Builder
	lineStart   int
	sectionFrom int
	fence       byte
	fenceLength int
	formatted   strings.Builder
	lastFrame   string
}

func (stream *textStream) renderAssistant(model *Model, width int) string {
	if stream.preview == nil || stream.preview.width != width {
		stream.preview = &markdownPreview{width: width}
	}
	p := stream.preview
	if p.consumed == stream.raw.Len() {
		return p.lastFrame
	}
	scan := p.plain.Len()
	p.plain.WriteString(p.sanitizer.Filter(stream.raw.String()[p.consumed:]))
	p.consumed = stream.raw.Len()
	source := p.plain.String()
	for {
		offset := strings.IndexByte(source[scan:], '\n')
		if offset < 0 {
			break
		}
		end := scan + offset
		if p.endsSection(source[p.lineStart:end]) {
			section := source[p.sectionFrom : end+1]
			if strings.TrimSpace(section) != "" {
				body := model.markdown.render(section, max(12, width-3))
				body = linkFilePaths(body, model.workspace)
				if body != "" {
					p.formatted.WriteString(p.rows(model, body))
				}
			}
			p.sectionFrom = end + 1
		}
		scan = end + 1
		p.lineStart = scan
	}
	tail := source[p.sectionFrom:]
	p.lastFrame = p.formatted.String()
	if tail != "" {
		p.lastFrame += p.rows(model, strings.ReplaceAll(tail, "\t", strings.Repeat(" ", codeTabWidth)))
	}
	return p.lastFrame
}

func (p *markdownPreview) endsSection(line string) bool {
	if p.fence != 0 {
		if !isMarkdownFenceClose(line, p.fence, p.fenceLength) {
			return false
		}
		p.fence = 0
		return true
	}
	if strings.TrimSpace(line) == "" {
		return true
	}
	marker, length, _, ok := parseMarkdownFence(line)
	if !ok {
		_, marker, length, _, ok = parseInlineMarkdownFence(line)
	}
	if ok {
		p.fence, p.fenceLength = marker, length
	}
	return false
}

func (p *markdownPreview) rows(model *Model, body string) string {
	body = scopeFileLinksByLine(ansi.Hardwrap(body, max(1, p.width-2), true))
	prefix := "\n\n  "
	if p.formatted.Len() == 0 {
		prefix = model.styles.assistant.Render("│") + " "
	}
	return prefix + strings.ReplaceAll(body, "\n", "\n  ")
}

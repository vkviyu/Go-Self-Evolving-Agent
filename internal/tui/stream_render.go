package tui

import (
	"strings"

	"github.com/vkviyu/go-self-evolving-agent/internal/render"
)

// A block can move when the transcript slice grows. Keep the append-only
// builders behind a pointer so neither their data nor their state is copied.
type textStream struct {
	raw     strings.Builder
	preview *markdownPreview
}

// appendDelta retains the raw delta and exposes the accumulated text for the current
// transcript block.
func (block *transcriptBlock) appendDelta(delta string) {
	if block.stream == nil {
		block.stream = new(textStream)
	}
	block.stream.raw.WriteString(delta)
	block.content = block.stream.raw.String()
}

// finishStream releases incremental rendering state while keeping the block's accumulated
// content.
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
	codeRows    strings.Builder // finished display rows inside the open fence
	formatted   strings.Builder
	lastFrame   string
}

// renderAssistant incorporates newly arrived bytes into the preview and rebuilds its
// layout when the terminal width changes.
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
		p.finishLine(model, source, end)
		scan = end + 1
		p.lineStart = scan
	}
	p.lastFrame = p.formatted.String()
	line := source[p.lineStart:]
	if p.fence != 0 {
		p.lastFrame += p.codeRows.String()
		if line != "" && !isMarkdownFenceClose(line, p.fence, p.fenceLength) {
			p.lastFrame += p.codeLine(model, line)
		}
	} else if tail := source[p.sectionFrom:]; tail != "" {
		if isATXHeading(line) {
			tail = source[p.sectionFrom:p.lineStart] + model.markdown.render(line, width-3)
		}
		p.lastFrame += p.rows(model, strings.ReplaceAll(tail, "\t", strings.Repeat(" ", codeTabWidth)))
	}
	return p.lastFrame
}

// finishLine commits completed Markdown sections and keeps open fenced code rows available
// for the next frame.
func (p *markdownPreview) finishLine(model *Model, source string, end int) {
	line := source[p.lineStart:end]
	if p.fence != 0 {
		if !isMarkdownFenceClose(line, p.fence, p.fenceLength) {
			p.codeRows.WriteString(p.codeLine(model, line))
			return
		}
		p.commit(model, source[p.sectionFrom:end+1])
		p.sectionFrom = end + 1
		p.fence = 0
		p.codeRows.Reset()
		return
	}
	if prefix, marker, length, _, ok := markdownFenceOpening(line); ok {
		open := p.lineStart + len(prefix)
		p.commit(model, source[p.sectionFrom:open])
		p.sectionFrom = open
		p.fence, p.fenceLength = marker, length
	} else if strings.TrimSpace(line) == "" || isATXHeading(line) {
		p.commit(model, source[p.sectionFrom:end+1])
		p.sectionFrom = end + 1
	}
}

// Only routes candidate heading lines to the Markdown parser for a live
// preview. The parser still handles escapes, inline styling and closing marks.
func isATXHeading(line string) bool {
	text := strings.TrimLeft(line, " ")
	if len(line)-len(text) > 3 {
		return false
	}
	rest := strings.TrimLeft(text, "#")
	level := len(text) - len(rest)
	return level >= 1 && level <= 6 && (rest == "" || rest[0] == ' ' || rest[0] == '\t')
}

// commit renders a completed Markdown section once and appends its terminal rows to the
// preview.
func (p *markdownPreview) commit(model *Model, section string) {
	body := model.markdown.render(section, p.width-3)
	if body != "" {
		p.formatted.WriteString(p.rows(model, linkFilePaths(body, model.workspace)))
	}
}

// codeLine wraps an unfinished block's code line within the panel gutter, retaining its
// background even on blank lines.
func (p *markdownPreview) codeLine(model *Model, line string) string {
	innerWidth := max(minimumMarkdownWidth, p.width-3) - 2
	body := codePanelRows(wrapCode(line, innerWidth), innerWidth)
	if p.codeRows.Len() == 0 {
		return p.rows(model, body)
	}
	return "\n" + markedTranscriptRows(body, "  ", p.width)
}

func (p *markdownPreview) rows(model *Model, body string) string {
	if p.formatted.Len() == 0 {
		return markedTranscriptRows(body, model.styles.assistant.Render("│")+" ", p.width)
	}
	return "\n\n" + markedTranscriptRows(body, "  ", p.width)
}

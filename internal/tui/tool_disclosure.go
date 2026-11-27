package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// An inspector projects a selection of tool blocks into the transcript viewport.
// The conversation keeps receiving events while the reader scrolls independently.
type toolInspector struct {
	indices   []int
	selected  int
	detail    bool
	diffsOnly bool
	reading   transcriptReadingState
}

func (model *Model) toggleToolDetails() {
	if model.toolInspector != nil {
		model.closeToolInspector()
		return
	}
	model.openToolInspector(0, len(model.displayBlocks()), false)
}

func (model *Model) openToolInspector(start, end int, diffsOnly bool) bool {
	if model.approvalPending != nil {
		return false
	}
	blocks := model.displayBlocks()
	view := &toolInspector{diffsOnly: diffsOnly, reading: transcriptReadingState{position: model.readingPosition(), follow: model.autoScroll}}
	for i := start; i < end; i++ {
		if isCompactToolBlock(&blocks[i]) && (!diffsOnly || hasDiffPayload(blocks[i].toolResult)) {
			view.indices = append(view.indices, i)
		}
	}
	if len(view.indices) == 0 {
		return false
	}
	view.detail = len(view.indices) == 1
	model.toolInspector = view
	model.resetToolPage()
	return true
}

// resetToolPage clears the transcript selection and starts the tool inspector at its first
// row.
func (model *Model) resetToolPage() {
	model.autoScroll = false
	model.selection = transcriptSelection{}
	model.transcriptLayout = nil
	model.viewport.GotoTop()
	model.refreshTranscript(false)
}

// closeToolInspector restores the transcript reading position saved on entry to the tool
// details view.
func (model *Model) closeToolInspector() {
	view := model.toolInspector
	if view == nil {
		return
	}
	model.toolInspector = nil
	model.resetToolPage()
	model.restoreReadingPosition(view.reading.position)
	model.autoScroll = view.reading.follow && model.viewport.AtBottom()
}

func (model *Model) backFromTool() {
	view := model.toolInspector
	if view.detail && len(view.indices) > 1 {
		view.detail = false
		model.resetToolPage()
		model.keepToolSelected()
		return
	}
	model.closeToolInspector()
}

func (model *Model) toolInspectionHeader(width int) string {
	view := model.toolInspector
	label := fmt.Sprintf("Tool calls (%d)", len(view.indices))
	if view.diffsOnly {
		label = fmt.Sprintf("Code diffs (%d)", len(view.indices))
	}
	hint := "↑↓ select · Enter open"
	if view.detail {
		hint = "↑↓ scroll"
	}
	return ansi.Truncate(model.styles.toolHeader.Render("‹ Back  "+label)+model.styles.metadata.Render("  ·  Esc back  ·  "+hint), width, "…")
}

func (model *Model) toolInspectionParts(blocks []transcriptBlock) []transcriptPart {
	view := model.toolInspector
	if view.detail {
		index := view.indices[view.selected]
		block := &blocks[index]
		rendered := model.renderBlockWithDetails(block, true, true)
		return []transcriptPart{{rendered: rendered, block: block, index: index, toolEnd: index + 1}}
	}
	parts := make([]transcriptPart, 0, len(view.indices))
	for row, index := range view.indices {
		block := &blocks[index]
		name := singleLinePreview(block.toolName, model.viewport.Width)
		if name == "" {
			name = "Tool result"
		}
		marker := "  "
		if row == view.selected {
			marker = "› "
		}
		if fail := block.failureSummary(); fail != "" {
			name = model.styles.failure.Render("! "+name) + "  " + singleLinePreview(fail, model.viewport.Width)
		} else {
			name = model.styles.toolHeader.Render(name) + "  " + model.styles.metadata.Render(toolArgumentPreview(block.toolArguments, model.viewport.Width))
		}
		parts = append(parts, transcriptPart{rendered: ansi.Truncate(marker+name, max(1, model.viewport.Width), "…"), block: block, index: index, toolEnd: index + 1})
	}
	return parts
}

// Prefer the target of common coding tools. The detail page always retains
// the full arguments for unfamiliar tools and for disambiguation.
func toolArgumentPreview(raw string, width int) string {
	var args struct{ Pattern, Path, Command string }
	// A write may contain megabytes of source. Never parse or preview that
	// payload just to paint a one-line list.
	if len(raw) <= 8192 && json.Unmarshal([]byte(raw), &args) == nil {
		return singleLinePreview(strings.TrimSpace(args.Pattern+" "+args.Path+" "+args.Command), width)
	}
	return ""
}

func (model *Model) toggleToolDisclosureAtLine(line int) bool {
	for _, span := range model.transcriptLayout {
		if span.start != line || span.toolEnd == 0 {
			continue
		}
		if view := model.toolInspector; view != nil {
			if view.detail {
				model.backFromTool()
			} else {
				// List rows are exactly one terminal line each.
				view.selected, view.detail = line, true
				model.resetToolPage()
			}
			return true
		}
		if model.approvalPending != nil {
			block := &model.displayBlocks()[span.blockIndex]
			if block.approvalState != approvalPending {
				return false
			}
			block.argumentsExpanded = !block.argumentsExpanded
			model.refreshTranscript(false)
			return true
		}
		return model.openToolInspector(span.blockIndex, span.toolEnd, false)
	}
	return false
}

func (model *Model) keepToolSelected() {
	row := model.toolInspector.selected
	if row < model.viewport.YOffset {
		model.viewport.SetYOffset(row)
	} else if row >= model.viewport.YOffset+model.viewport.Height {
		model.viewport.SetYOffset(row - model.viewport.Height + 1)
	}
}

func (model *Model) handleToolInspectionKey(msg tea.KeyMsg) bool {
	view := model.toolInspector
	switch {
	case key.Matches(msg, toggleToolBinding):
		model.closeToolInspector()
	case msg.Type == tea.KeyEsc:
		model.backFromTool()
	case msg.Type == tea.KeyEnter:
		if !view.detail {
			view.detail = true
			model.resetToolPage()
		}
	case msg.Type == tea.KeyUp || msg.Type == tea.KeyDown || msg.Type == tea.KeyPgUp || msg.Type == tea.KeyPgDown:
		step := 1
		if msg.Type == tea.KeyPgUp || msg.Type == tea.KeyPgDown {
			step = max(1, model.viewport.Height-1)
		}
		if msg.Type == tea.KeyUp || msg.Type == tea.KeyPgUp {
			step = -step
		}
		if view.detail {
			model.viewport.SetYOffset(model.viewport.YOffset + step)
		} else {
			view.selected = max(0, min(len(view.indices)-1, view.selected+step))
			model.refreshTranscript(false)
			model.keepToolSelected()
		}
	default:
		// Typing returns focus to the existing draft. Cancellation and other
		// conversation shortcuts retain their normal behavior.
		model.closeToolInspector()
		return false
	}
	return true
}

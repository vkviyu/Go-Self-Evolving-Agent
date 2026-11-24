package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/render"
)

type blockKind uint8

const (
	blockUser blockKind = iota
	blockAssistant
	blockReasoning
	blockTool
	blockSystem
	blockFailure
	blockApproval
	// blockBackground shows Agent messages that the runtime gave the root
	// Agent as background data.
	blockBackground
)

const (
	approvalAllow = iota
	approvalReject
)

type approvalState uint8

const (
	approvalNone approvalState = iota
	approvalPending
	approvalApproved
	approvalRejected
)

type transcriptBlock struct {
	kind           blockKind
	content        string
	toolName       string
	toolArguments  string
	toolCallID     string
	toolResult     string
	approvalReason string
	// approvalSource names the delegated Agent that asked; it is set once
	// when the approval card is created, so it is not part of the cache key.
	approvalSource    string
	argumentsExpanded bool
	resultExpanded    bool
	pending           bool
	approvalState     approvalState

	cachedContent           string
	cachedToolName          string
	cachedToolArguments     string
	cachedToolResult        string
	cachedFailureName       string
	cachedFailureResult     string
	cachedFailure           string
	cachedApprovalReason    string
	cachedArgumentsExpanded bool
	cachedResultExpanded    bool
	cachedWidth             int
	cachedApproval          approvalState
	cachedApprovalChoice    int
	cachedOutput            string
}

type transcriptPart struct {
	rendered string
	block    *transcriptBlock
	index    int
	toolEnd  int // exclusive block index for a clickable tool header
}

func blocksFromHistory(messages []*schema.Message) []transcriptBlock {
	blocks := make([]transcriptBlock, 0, len(messages))
	toolBlocks := make(map[string]int)
	for _, message := range messages {
		if message == nil {
			continue
		}
		switch message.Role {
		case schema.User:
			kind := blockUser
			if isBackgroundInput(message.Content) {
				kind = blockBackground
			}
			blocks = append(blocks, transcriptBlock{kind: kind, content: message.Content})
		case schema.Assistant:
			if message.Content != "" {
				blocks = append(blocks, transcriptBlock{kind: blockAssistant, content: message.Content})
			}
			for _, call := range message.ToolCalls {
				blocks = append(blocks, transcriptBlock{
					kind:          blockTool,
					content:       call.Function.Name,
					toolName:      call.Function.Name,
					toolArguments: call.Function.Arguments,
					toolCallID:    call.ID,
				})
				if call.ID != "" {
					toolBlocks[call.ID] = len(blocks) - 1
				}
			}
		case schema.Tool:
			if index, ok := toolBlocks[message.ToolCallID]; ok {
				blocks[index].toolResult = message.Content
				blocks[index].pending = false
				continue
			}
			content := message.Content
			toolResult := ""
			if message.ToolName != "" {
				content = message.ToolName
				toolResult = message.Content
			}
			block := transcriptBlock{kind: blockTool, content: content, toolName: message.ToolName, toolResult: toolResult}
			blocks = append(blocks, block)
		}
	}
	return blocks
}

func (model *Model) refreshTranscript(forceBottom bool) {
	if !model.ready {
		return
	}
	if forceBottom && model.toolInspector == nil {
		model.autoScroll = true
	}
	follow := model.autoScroll && model.toolInspector == nil
	position := model.readingPosition()
	transcriptParts := model.transcriptParts()
	content, layout, queries := layoutTranscriptParts(transcriptParts)
	if model.transcriptContent != content {
		model.selection = transcriptSelection{}
	}
	model.transcriptContent = content
	model.transcriptLayout, model.queryAnchors = layout, queries
	model.viewport.SetContent(content)
	if follow {
		model.viewport.GotoBottom()
	} else {
		model.restoreReadingPosition(position)
	}
}

func (model *Model) transcriptParts() []transcriptPart {
	blocks := model.displayBlocks()
	if model.toolInspector != nil {
		return model.toolInspectionParts(blocks)
	}
	parts := make([]transcriptPart, 0, len(blocks))
	for index := 0; index < len(blocks); index++ {
		block := &blocks[index]
		if block.kind == blockReasoning && !model.showReasoning {
			continue
		}
		if isCompletedToolBlock(block) {
			end := index + 1
			for end < len(blocks) && isCompletedToolBlock(&blocks[end]) {
				end++
			}
			if end-index > 1 {
				parts = append(parts, transcriptPart{
					rendered: ansi.Hardwrap(model.renderToolGroupSummary(blocks[index:end]), max(1, model.viewport.Width), true),
					block:    block,
					index:    index,
					toolEnd:  end,
				})
				index = end - 1
				continue
			}
		}
		if rendered := model.renderBlock(block); rendered != "" {
			// Glamour wraps words, so long unspaced CJK paragraphs can remain
			// wider than its wrap setting. Viewport clips such lines horizontally;
			// normalize cell width here before counting lines or storing anchors.
			rendered = scopeFileLinksByLine(ansi.Hardwrap(rendered, max(1, model.viewport.Width), true))
			part := transcriptPart{rendered: rendered, block: block, index: index}
			if isCompactToolBlock(block) && (block.toolArguments != "" || block.toolResult != "") {
				part.toolEnd = index + 1
			}
			parts = append(parts, part)
		}
	}
	return parts
}

func layoutTranscriptParts(parts []transcriptPart) (string, []transcriptSpan, []queryAnchor) {
	var content strings.Builder
	spans := make([]transcriptSpan, 0, len(parts))
	var queries []queryAnchor
	line := 0
	for index, part := range parts {
		if index > 0 {
			separator := transcriptSeparator(parts[index-1].block, part.block)
			content.WriteString(separator)
			line += strings.Count(separator, "\n")
		}
		height := strings.Count(part.rendered, "\n") + 1
		spans = append(spans, transcriptSpan{blockIndex: part.index, start: line, end: line + height, toolEnd: part.toolEnd})
		if part.block != nil && part.block.kind == blockUser {
			queries = append(queries, queryAnchor{start: line, end: line + height, preview: singleLinePreview(part.block.content, 160)})
		}
		content.WriteString(part.rendered)
		line += height - 1
	}
	return content.String(), spans, queries
}

func isCompletedToolBlock(block *transcriptBlock) bool {
	return block != nil &&
		(block.kind == blockTool || block.kind == blockApproval) &&
		!block.pending &&
		block.approvalState != approvalPending
}

func (model *Model) renderToolGroupSummary(group []transcriptBlock) string {
	failures := 0
	counts := make(map[string]int)
	var names []string
	for index := range group {
		block := &group[index]
		name := strings.TrimSpace(block.toolName)
		if name == "" {
			name = "Tool result"
		}
		if counts[name] == 0 {
			names = append(names, name)
		}
		counts[name]++
		if block.failureSummary() != "" {
			failures++
		}
	}
	label := fmt.Sprintf("%d tool calls", len(group))
	if failures > 0 {
		label += fmt.Sprintf(" · %d failed", failures)
	}
	var previews []string
	for _, name := range names[:min(len(names), 3)] {
		preview := singleLinePreview(name, 45)
		if counts[name] > 1 {
			preview += fmt.Sprintf(" ×%d", counts[name])
		}
		previews = append(previews, preview)
	}
	if len(names) > len(previews) {
		previews = append(previews, "…")
	}
	if len(previews) > 0 {
		label += " · " + strings.Join(previews, ", ")
	}
	// Keep the disclosure and failure count visible even in a narrow terminal.
	label = ansi.Truncate(label, max(1, model.viewport.Width-5), "…")
	return model.styles.toolHeader.Render("• " + label + "  ▸")
}

func joinTranscriptParts(parts []string, blocks []*transcriptBlock) string {
	if len(parts) == 0 {
		return ""
	}
	var content strings.Builder
	content.WriteString(parts[0])
	for index := 1; index < len(parts); index++ {
		content.WriteString(transcriptSeparator(blocks[index-1], blocks[index]))
		content.WriteString(parts[index])
	}
	return content.String()
}

func transcriptSeparator(previous, current *transcriptBlock) string {
	if isCompactToolBlock(previous) && isCompactToolBlock(current) {
		return "\n"
	}
	return "\n\n"
}

func isCompactToolBlock(block *transcriptBlock) bool {
	return block != nil && (block.kind == blockTool || block.kind == blockApproval)
}

func (model *Model) renderViewport() string {
	content := model.viewport.View()
	if !model.selection.active {
		return content
	}
	start, end, startColumn, endColumn := model.selectionBounds()
	start -= model.viewport.YOffset
	end -= model.viewport.YOffset
	lines := strings.Split(content, "\n")
	for index := max(0, start); index <= min(end, len(lines)-1); index++ {
		lineStart, lineEnd := 0, ansi.StringWidth(lines[index])
		if index == start {
			lineStart = startColumn
		}
		if index == end {
			lineEnd = endColumn
		}
		lines[index] = highlightSelection(lines[index], lineStart, lineEnd, model.styles.selection)
	}
	return strings.Join(lines, "\n")
}

func (model *Model) selectionBounds() (start, end, startColumn, endColumn int) {
	start, end = model.selection.start, model.selection.end
	startColumn, endColumn = model.selection.startColumn, model.selection.endColumn
	if start > end || (start == end && startColumn > endColumn) {
		start, end = end, start
		startColumn, endColumn = endColumn, startColumn
	}
	return
}

func highlightSelection(line string, left, right int, style lipgloss.Style) string {
	plain := ansi.Strip(line)
	width := ansi.StringWidth(plain)
	left = max(0, min(left, width))
	right = max(0, min(right, width))
	if right < left {
		left, right = right, left
	}
	if left == right {
		return line
	}
	selected := ansi.Cut(plain, left, right)
	rendered := style.Render(selected)
	if rendered == selected {
		rendered = "\x1b[48;5;238m" + selected + "\x1b[0m"
	}
	return ansi.Cut(plain, 0, left) + rendered + ansi.Cut(plain, right, width)
}

func (model *Model) renderBlock(block *transcriptBlock) string {
	return model.renderBlockWithDetails(block, block.argumentsExpanded, block.resultExpanded)
}

func (model *Model) renderBlockWithDetails(block *transcriptBlock, arguments, result bool) string {
	width := max(12, model.viewport.Width-3)
	if block.cachedContent == block.content && block.cachedToolName == block.toolName && block.cachedToolArguments == block.toolArguments && block.cachedToolResult == block.toolResult && block.cachedApprovalReason == block.approvalReason && block.cachedArgumentsExpanded == arguments && block.cachedResultExpanded == result && block.cachedWidth == width && block.cachedApproval == block.approvalState && (block.approvalState != approvalPending || block.cachedApprovalChoice == model.approvalChoice) {
		return block.cachedOutput
	}
	// Sanitize a private presentation view, never the durable transcript. Host
	// styling and file links are applied after removing untrusted commands.
	original := block
	safe := *block
	safe.argumentsExpanded, safe.resultExpanded = arguments, result
	safe.content = render.PlainText(block.content)
	safe.toolName = render.PlainText(block.toolName)
	safe.toolArguments = render.PlainText(block.toolArguments)
	safe.toolResult = render.PlainText(block.toolResult)
	safe.approvalReason = render.PlainText(block.approvalReason)
	safe.approvalSource = render.PlainText(block.approvalSource)
	block = &safe
	marker := "|"
	markerStyle := model.styles.system
	body := block.content
	switch block.kind {
	case blockUser:
		body = ansi.Hardwrap(block.content, max(1, width-2), false)
		body = model.styles.userMarker.Render("›") + model.styles.userPanel.Render(body)
		marker = ""
	case blockAssistant:
		marker = "│"
		markerStyle = model.styles.assistant
		body = linkFilePaths(model.markdown.render(block.content, width), model.workspace)
	case blockReasoning:
		body = ansi.Hardwrap(block.content, max(1, width-2), false)
		body = model.styles.reasoningPanel.Padding(0).Render("┊") + model.styles.reasoningPanel.Render(body)
		marker = ""
	case blockTool:
		marker = "•"
		markerStyle = model.styles.tool
		body = model.renderToolBody(block, width)
		if block.approvalState != approvalNone {
			marker, markerStyle, body = model.renderApprovalState(block.approvalState, body)
		}
	case blockBackground:
		marker = "◆"
		markerStyle = model.styles.backgroundHeader
		body = model.renderBackgroundBody(block.content, width)
	case blockFailure:
		marker = "!"
		markerStyle = model.styles.failure
		body = ansi.Hardwrap(block.content, width, false)
	case blockApproval:
		body = model.renderToolBody(block, width)
		state := block.approvalState
		if state == approvalNone {
			state = approvalPending
		}
		marker, markerStyle, body = model.renderApprovalState(state, body)
	default:
		marker = "·"
		body = ansi.Hardwrap(block.content, width, false)
	}
	if block.kind == blockTool || block.kind == blockApproval {
		if failure := original.failureSummary(); failure != "" {
			// Permission to execute is not evidence of successful execution.
			// Keep failures visible even when the argument/result panes collapse.
			marker, markerStyle = "!", model.styles.failure
			body += "\n" + model.styles.failure.Render(ansi.Hardwrap("  failed: "+render.PlainText(failure), width, false))
		}
	}
	output := body
	if marker != "" {
		output = lipgloss.JoinHorizontal(lipgloss.Top, markerStyle.Render(marker)+" ", body)
	}
	block = original
	block.cachedContent = block.content
	block.cachedToolName = block.toolName
	block.cachedToolArguments = block.toolArguments
	block.cachedToolResult = block.toolResult
	block.cachedApprovalReason = block.approvalReason
	block.cachedArgumentsExpanded = arguments
	block.cachedResultExpanded = result
	block.cachedWidth = width
	block.cachedApproval = block.approvalState
	block.cachedApprovalChoice = model.approvalChoice
	block.cachedOutput = output
	return output
}

func (model *Model) renderToolBody(block *transcriptBlock, width int) string {
	toolName := strings.TrimSpace(block.toolName)
	if toolName == "" {
		toolName = strings.TrimSpace(block.content)
	}
	arguments := strings.TrimSpace(block.toolArguments)
	result := strings.TrimSpace(block.toolResult)
	hasDetails := arguments != "" || result != ""
	header := toolName
	if hasDetails {
		indicator := "▸"
		if block.argumentsExpanded || block.resultExpanded {
			indicator = "▾"
		}
		header = toolName + "  " + indicator
	}
	parts := []string{model.styles.toolHeader.Render(header)}
	if source := strings.TrimSpace(block.approvalSource); source != "" {
		parts = append(parts, model.styles.metadata.Render(ansi.Hardwrap("  requested by "+source, max(1, width), false)))
	}
	if block.argumentsExpanded && arguments != "" {
		// Arguments that carry a diff, such as a merge plan, read as a diff.
		formatted := arguments
		if hasDiffPayload(arguments) {
			formatted = formatToolResult(arguments)
		}
		parts = append(parts, model.renderToolSection("▾ arguments", formatted, width, model.styles.toolArguments))
	}
	if reason := strings.TrimSpace(block.approvalReason); reason != "" {
		parts = append(parts, model.renderApprovalReason(reason, width))
	}
	if block.resultExpanded && result != "" {
		parts = append(parts, model.renderToolSection("▾ result", formatToolResult(result), width, model.styles.toolResult))
	}
	return strings.Join(parts, "\n")
}

func hasDiffPayload(result string) bool {
	var payload map[string]json.RawMessage
	if json.Unmarshal([]byte(result), &payload) != nil {
		return false
	}
	if raw, ok := payload["diff"]; ok {
		var item struct {
			Status      string `json:"status"`
			Content     string `json:"content"`
			Preview     string `json:"preview"`
			ArtifactRef string `json:"artifact_ref"`
			Message     string `json:"message"`
		}
		if json.Unmarshal(raw, &item) == nil && (item.Content != "" || item.Preview != "" || item.ArtifactRef != "" || item.Message != "" || (item.Status != "" && item.Status != "unchanged")) {
			return true
		}
	}
	var items []struct {
		Status      string `json:"status"`
		Content     string `json:"content"`
		Preview     string `json:"preview"`
		ArtifactRef string `json:"artifact_ref"`
		Message     string `json:"message"`
	}
	if raw, ok := payload["diffs"]; ok && json.Unmarshal(raw, &items) == nil {
		for _, item := range items {
			if item.Content != "" || item.Preview != "" || item.ArtifactRef != "" || item.Message != "" || (item.Status != "" && item.Status != "unchanged") {
				return true
			}
		}
	}
	return false
}

func formatToolResult(result string) string {
	var payload map[string]json.RawMessage
	if json.Unmarshal([]byte(result), &payload) != nil {
		return formatJSON(result)
	}
	diffParts := make([]string, 0)
	if raw, ok := payload["diff"]; ok {
		if rendered := formatDiffPayload(raw); rendered != "" {
			diffParts = append(diffParts, rendered)
		}
		delete(payload, "diff")
	}
	if raw, ok := payload["diffs"]; ok {
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) == nil {
			for _, item := range items {
				if rendered := formatDiffPayload(item); rendered != "" {
					diffParts = append(diffParts, rendered)
				}
			}
		}
		delete(payload, "diffs")
	}
	base, err := json.Marshal(payload)
	if err != nil {
		return formatJSON(result)
	}
	parts := make([]string, 0, 1+len(diffParts))
	if len(payload) > 0 {
		parts = append(parts, formatJSON(string(base)))
	}
	parts = append(parts, diffParts...)
	if len(parts) == 0 {
		return formatJSON(result)
	}
	return strings.Join(parts, "\n")
}

func formatDiffPayload(raw json.RawMessage) string {
	var item struct {
		Path        string `json:"path"`
		Status      string `json:"status"`
		Content     string `json:"content"`
		Preview     string `json:"preview"`
		ArtifactRef string `json:"artifact_ref"`
		Message     string `json:"message"`
	}
	if json.Unmarshal(raw, &item) != nil {
		return ""
	}
	text := item.Content
	if text == "" {
		text = item.Preview
	}
	header := "▾ diff"
	if item.Path != "" {
		header += " " + item.Path
	}
	if item.Status != "" {
		header += " [" + item.Status + "]"
	}
	if text == "" {
		if item.ArtifactRef != "" {
			text = "full diff artifact: " + item.ArtifactRef
		}
		if item.Message != "" {
			if text != "" {
				text += "\n"
			}
			text += item.Message
		}
	}
	if text == "" {
		return header
	}
	return header + "\n" + text
}

func (model *Model) renderApprovalReason(reason string, width int) string {
	const label = " ? approval reason:"
	available := max(1, width-ansi.StringWidth(label)-1)
	wrapped := ansi.Hardwrap(reason, available, false)
	lines := strings.Split(wrapped, "\n")
	if len(lines) == 0 {
		return model.styles.approval.Render(label)
	}
	output := []string{model.styles.approval.Render(label) + " " + model.styles.approvalReason.Render(lines[0])}
	for _, line := range lines[1:] {
		output = append(output, model.styles.approvalReason.Render("    "+line))
	}
	return strings.Join(output, "\n")
}

func (model *Model) renderToolSection(label, content string, width int, style lipgloss.Style) string {
	content = render.PlainText(content)
	content = linkFilePaths(content, model.workspace)
	content = ansi.Hardwrap(content, max(1, width-4), true)
	content = indentLines(content, "    ")
	return style.Render("  "+label) + "\n" + style.Render(content)
}

func indentLines(content, prefix string) string {
	lines := strings.Split(content, "\n")
	for index := range lines {
		lines[index] = prefix + lines[index]
	}
	return strings.Join(lines, "\n")
}

func (model *Model) renderApprovalState(state approvalState, body string) (string, lipgloss.Style, string) {
	marker := "?"
	markerStyle := model.styles.approval
	status := "approval required"
	statusStyle := model.styles.approval
	switch state {
	case approvalApproved:
		marker = "✓"
		markerStyle = model.styles.approved
		status = "approved"
		statusStyle = model.styles.approved
	case approvalRejected:
		marker = "×"
		markerStyle = model.styles.rejected
		status = "rejected"
		statusStyle = model.styles.rejected
	}
	if state == approvalPending && model.approvalPending != nil {
		body = lipgloss.JoinVertical(lipgloss.Left, body, model.renderApprovalOptions())
		// The status bar owns the active "approval required" label and key
		// hints. Keep the card focused on the reason and decision controls;
		// resolved decisions still get a persistent status below.
		return marker, markerStyle, body
	}
	if state == approvalApproved {
		// The check mark already communicates the durable approval decision.
		// Keep the completed tool call to one compact row; failures still add
		// their own explicit summary below.
		return marker, markerStyle, body
	}
	label := "status: "
	if state != approvalPending {
		label = "approval: "
	}
	return marker, markerStyle, lipgloss.JoinVertical(lipgloss.Left, body, statusStyle.Render("  "+label+status))
}

func (model *Model) renderApprovalOptions() string {
	allow := model.styles.approvalOption.Render("    Allow")
	reject := model.styles.approvalOption.Render("    Reject")
	if model.approvalChoice == approvalAllow {
		allow = model.styles.approvalSelected.Render("  › Allow")
	} else {
		reject = model.styles.approvalSelected.Render("  › Reject")
	}
	return lipgloss.JoinVertical(lipgloss.Left, allow, reject)
}

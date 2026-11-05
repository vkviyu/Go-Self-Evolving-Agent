package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

func (model *Model) toggleToolDetails() {
	allExpanded := true
	hasDetails := false
	blocks := model.displayBlocks()
	for index := range blocks {
		block := &blocks[index]
		if block.kind != blockTool && block.kind != blockApproval {
			continue
		}
		if block.toolArguments != "" {
			hasDetails = true
			allExpanded = allExpanded && block.argumentsExpanded
		}
		if block.toolResult != "" {
			hasDetails = true
			allExpanded = allExpanded && block.resultExpanded
		}
		if isCompletedToolBlock(block) {
			hasDetails = true
			allExpanded = allExpanded && block.toolGroupExpanded
		}
	}
	if !hasDetails {
		return
	}
	expand := !allExpanded
	for index := range blocks {
		block := &blocks[index]
		if block.kind != blockTool && block.kind != blockApproval {
			continue
		}
		if block.toolArguments != "" {
			block.argumentsExpanded = expand
		}
		if block.toolResult != "" {
			block.resultExpanded = expand
		}
		if isCompletedToolBlock(block) {
			block.toolGroupExpanded = expand
		}
	}
}

func (model *Model) toggleToolDisclosureAtLine(target int) bool {
	lineOffset := 0
	parts := model.transcriptParts()
	for index, part := range parts {
		block := part.block
		lines := strings.Split(part.rendered, "\n")
		if target >= lineOffset && target < lineOffset+len(lines) {
			line := strings.TrimSpace(ansi.Strip(lines[target-lineOffset]))
			if block == nil {
				return false
			}
			if isCompletedToolBlock(block) && strings.HasSuffix(line, "▸") && strings.Contains(line, "tool calls") {
				block.toolGroupExpanded = true
				return true
			}
			toolName := strings.TrimSpace(block.toolName)
			if toolName == "" {
				toolName = strings.TrimSpace(block.content)
			}
			switch {
			case strings.HasSuffix(line, toolName+"  ▸") || strings.HasSuffix(line, toolName+" ▸"):
				block.argumentsExpanded = block.toolArguments != ""
				block.resultExpanded = block.toolResult != ""
				return true
			case strings.HasSuffix(line, toolName+"  ▾") || strings.HasSuffix(line, toolName+" ▾"):
				block.argumentsExpanded = false
				block.resultExpanded = false
				return true
			case strings.HasPrefix(line, "▸ arguments"):
				block.argumentsExpanded = true
				return true
			case strings.HasPrefix(line, "▾ arguments"):
				block.argumentsExpanded = false
				return true
			case strings.HasPrefix(line, "▸ result"):
				block.resultExpanded = true
				return true
			case strings.HasPrefix(line, "▾ result"):
				block.resultExpanded = false
				return true
			}
		}
		lineOffset += len(lines)
		if index < len(parts)-1 {
			lineOffset += strings.Count(transcriptSeparator(part.block, parts[index+1].block), "\n") - 1
		}
	}
	return false
}

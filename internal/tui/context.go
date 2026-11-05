package tui

import (
	"fmt"
	"strings"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/modelcontext"
)

func (model *Model) contextUsageDescription() string {
	usage := model.contextUsage
	capacity := "unknown (not declared)"
	if usage.Capacity.Source == modelcontext.SourceFallback {
		capacity = fmt.Sprintf("unknown (fallback reference: %d tokens; not a known model capacity)", usage.Capacity.Window)
	} else if usage.Capacity.Window > 0 {
		capacity = fmt.Sprintf("%d tokens (%s)", usage.Capacity.Window, usage.Capacity.Source)
	}
	maxOutput := "unknown (not declared)"
	if usage.Capacity.MaxOutputTokens > 0 {
		maxOutput = fmt.Sprintf("%d tokens (%s)", usage.Capacity.MaxOutputTokens, usage.Capacity.OutputSource)
	}
	lines := []string{
		fmt.Sprintf("Full input: %d / %d tokens (%s)", usage.Used, usage.InputLimit, usage.Status),
		fmt.Sprintf("Working window: %d tokens (%s)", usage.Window, usage.WindowSource),
		"Model capacity: " + capacity,
		"Max output: " + maxOutput,
		fmt.Sprintf("Output reserve: %d tokens", usage.OutputReserve),
		fmt.Sprintf("Input budget: %d tokens", usage.InputLimit),
		fmt.Sprintf("Auto-compaction trigger: %d tokens", usage.Trigger),
		"State: " + model.contextStateLabel(),
	}
	if usage.NeedsCompaction || usage.State == agentruntime.ContextCompactionPending || usage.State == agentruntime.ContextOverBudget {
		lines = append(lines, "Compaction will be checked before the next model request.", "No idle model request is made.")
	}
	lines = append(lines, "Input includes system instructions and tool schemas. Output and safety space are reserved once. Cached input still occupies context. Counts describe the current full input, not cumulative session usage.")
	return strings.Join(lines, "\n")
}

func (model *Model) contextStateLabel() string {
	if model.contextUsage.State != "" {
		return string(model.contextUsage.State)
	}
	if model.contextUsage.NeedsCompaction {
		return string(agentruntime.ContextCompactionPending)
	}
	return string(agentruntime.ContextReady)
}

func (model *Model) contextPendingHint(compact bool) string {
	if model.contextUsage.State == agentruntime.ContextOverBudget {
		if compact {
			return "over"
		}
		return "over budget"
	}
	if model.contextUsage.NeedsCompaction || model.contextUsage.State == agentruntime.ContextCompactionPending {
		if compact {
			return "pending"
		}
		return "compact pending"
	}
	return ""
}

func (model *Model) contextBudgetLabel() string {
	if model.contextUsage.InputLimit <= 0 {
		return "?"
	}
	return formatTokens(model.contextUsage.InputLimit)
}

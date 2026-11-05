package agentruntime

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
)

// goalToolOptions only shapes model-visible tools. The immutable Eino tool
// handlers stay registered so sequential calls in one batch still work. Actual
// enforcement belongs to the pipeline's turn guard, not to tool visibility.
func (m *recordingModel) goalToolOptions(ctx context.Context, options []model.Option) ([]model.Option, error) {
	if m.factory == nil || !m.factory.goalMode.Enabled || m.factory.goalMode.Activation != agentgoal.ActivationAuto {
		return options, nil
	}
	common := model.GetCommonOptions(nil, options...)
	if len(common.Tools) == 0 {
		return options, nil // independent evaluator has no tools
	}
	goal, _, found, err := m.factory.goalStore.GoalForTurn(ctx, m.sessionID, m.turnID)
	if err != nil {
		return nil, fmt.Errorf("resolve model task phase: %w", err)
	}
	active := found && goal.State == agentgoal.StateRunning
	filtered := make([]*schema.ToolInfo, 0, len(common.Tools))
	for _, info := range common.Tools {
		if (info.Name == agentgoal.ReportToolName && !active) || (info.Name == agentgoal.StartToolName && active) {
			continue
		}
		filtered = append(filtered, info)
	}
	return append(append([]model.Option(nil), options...), model.WithTools(filtered)), nil
}

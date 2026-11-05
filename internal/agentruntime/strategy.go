package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/strategy"
)

func (f *Factory) BaselineConfigID() string {
	if f.config.StrategyScope == "" {
		return f.executionConfigID()
	}
	return "strategy-baseline:" + strategy.Digest([]string{f.executionConfigID(), f.config.StrategyScope})
}

func (f *Factory) withActiveStrategy(ctx context.Context) (*Factory, error) {
	if f.strategyPinned || f.config.Strategies == nil {
		return f, nil
	}
	spec, err := f.config.Strategies.Active(ctx, f.BaselineConfigID())
	if err != nil {
		return nil, err
	}
	if spec == nil {
		return f, nil
	}
	return f.withStrategy(*spec)
}

func (f *Factory) withStrategy(spec strategy.Spec) (*Factory, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	selected := *f
	selected.strategyPinned, selected.strategyID, selected.baselineConfigID = true, spec.ID(), f.BaselineConfigID()
	selected.config.Instruction = strings.TrimSpace(f.config.Instruction) + "\n\nHost-reviewed task strategy (does not change authorization or acceptance criteria):\n" + spec.InstructionSuffix
	if spec.MaxIterations > 0 && spec.MaxIterations < selected.config.MaxIterations {
		selected.config.MaxIterations = spec.MaxIterations
	}
	if spec.MaxOutputTokens > 0 && spec.MaxOutputTokens < selected.config.MaxOutputTokens {
		selected.config.MaxOutputTokens = spec.MaxOutputTokens
	}
	return &selected, nil
}

func (f *Factory) withTurnStrategy(ctx context.Context, id session.ID, turnID string) (*Factory, error) {
	if f.strategyPinned {
		return f, nil
	}
	if err := f.CheckTurnRecovery(ctx, id, turnID); err != nil {
		return nil, err
	}
	var meta struct {
		SpecID string `json:"loop_spec_id"`
		BaseID string `json:"base_execution_config_id"`
	}
	err := session.VisitEvents(ctx, f.events, id, func(event session.Event) error {
		if event.Type == session.EventTurnStarted && event.CorrelationID == turnID {
			return json.Unmarshal(event.Payload, &meta)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if meta.SpecID == "" {
		return f, nil
	}
	if f.config.Strategies == nil {
		return nil, errors.New("old turn requires its immutable LoopSpec store")
	}
	if meta.BaseID != f.BaselineConfigID() {
		return nil, fmt.Errorf("%w: base config differs from pinned strategy turn", ErrTurnModelChanged)
	}
	spec, err := f.config.Strategies.Load(ctx, meta.SpecID)
	if err != nil {
		return nil, err
	}
	return f.withStrategy(spec)
}

func (f *Factory) strategyFailure() func(context.Context, error) error {
	if f.strategyID == "" || f.config.Strategies == nil {
		return nil
	}
	return func(ctx context.Context, cause error) error {
		return f.config.Strategies.Fallback(ctx, f.baselineConfigID, f.strategyID, fmt.Sprintf("turn execution failed: %.1024s; no automatic replay", cause.Error()))
	}
}

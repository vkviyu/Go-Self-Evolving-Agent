package evolutiontools

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/vkviyu/go-self-evolving-agent/internal/strategy"
)

func (adapter strategyAdapter) propose(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	caller, err := adapter.caller(ctx)
	if err != nil {
		return nil, err
	}
	var spec strategy.Spec
	if err := decodeEvolutionArgs(args, &spec); err != nil {
		return nil, err
	}
	spec.OwnerSession = caller.OwnerSession
	id, err := adapter.store.Save(ctx, spec)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"candidate_id": id, "state": "candidate"})
}

func (adapter strategyAdapter) inspect(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	scope, err := adapter.scopedStrategy(ctx, args)
	if err != nil {
		return nil, err
	}
	if scope.input.ReportID != "" {
		return json.Marshal(scope.report)
	}
	return json.Marshal(scope.spec)
}

func (adapter strategyAdapter) shadow(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	scope, err := adapter.scopedStrategy(ctx, args)
	if err != nil {
		return nil, err
	}
	if scope.input.SuiteID == "" || scope.input.ReportID != "" {
		return nil, errors.New("shadow requires candidate_id and suite_id only")
	}
	report, id, err := scope.caller.Compare(ctx, scope.input.CandidateID, scope.input.SuiteID)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"report_id": id, "report": report})
}

func (adapter strategyAdapter) publish(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	scope, err := adapter.scopedStrategy(ctx, args)
	if err != nil {
		return nil, err
	}
	if scope.input.ReportID == "" || scope.input.CandidateID != "" || scope.input.SuiteID != "" {
		return nil, errors.New("publish requires report_id only")
	}
	if scope.report.BaselineConfigID != scope.caller.BaselineConfigID {
		return nil, errors.New("report belongs to a different baseline config")
	}
	if err := adapter.store.Promote(ctx, scope.input.ReportID, true); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"report_id": scope.input.ReportID, "available_next_turn": true})
}

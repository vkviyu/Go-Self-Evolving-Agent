package evolutiontools

import (
	"context"
	"errors"

	"github.com/vkviyu/go-self-evolving-agent/internal/strategy"
)

type (
	strategyInput struct {
		CandidateID string `json:"candidate_id,omitempty"`
		SuiteID     string `json:"suite_id,omitempty"`
		ReportID    string `json:"report_id,omitempty"`
	}

	// strategyScope is a decoded strategy request whose candidate is owned by
	// the calling session.
	strategyScope struct {
		input  strategyInput
		spec   strategy.Spec
		caller StrategyCaller
		// report is loaded only when input names a report_id.
		report strategy.Report
	}
)

// scopedStrategy decodes args, resolves the candidate directly or through its
// report, and rejects candidates owned by another session.
func (adapter strategyAdapter) scopedStrategy(ctx context.Context, args []byte) (strategyScope, error) {
	var scope strategyScope
	if err := decodeEvolutionArgs(args, &scope.input); err != nil {
		return strategyScope{}, err
	}
	caller, err := adapter.caller(ctx)
	if err != nil {
		return strategyScope{}, err
	}
	id := scope.input.CandidateID
	if scope.input.ReportID != "" {
		if id != "" || scope.input.SuiteID != "" {
			return strategyScope{}, errors.New("report_id cannot be combined with candidate_id/suite_id")
		}
		scope.report, err = adapter.store.Report(ctx, scope.input.ReportID)
		if err != nil {
			return strategyScope{}, err
		}
		id = scope.report.CandidateID
	}
	if id == "" {
		return strategyScope{}, errors.New("candidate_id or report_id is required")
	}
	scope.spec, err = adapter.store.Load(ctx, id)
	if err != nil {
		return strategyScope{}, err
	}
	if scope.spec.OwnerSession != caller.OwnerSession {
		return strategyScope{}, errors.New("strategy belongs to a different session")
	}
	scope.caller = caller
	return scope, nil
}

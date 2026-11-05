package agentgoal

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

type Evaluation struct {
	Goal      Goal
	Round     Round
	Candidate string
	Report    *Report
	Evidence  []string
}

type Verdict struct {
	Decision Decision `json:"status"`
	Reason   string   `json:"reason"`
	Feedback string   `json:"feedback,omitempty"`
}

func (verdict Verdict) Validate() error {
	if err := verdict.Decision.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(verdict.Reason) == "" {
		return errors.New("goal verdict reason is required")
	}
	return nil
}

type Evaluator interface {
	Evaluate(context.Context, Evaluation) (Verdict, error)
}

type EvaluatorFunc func(context.Context, Evaluation) (Verdict, error)

func (function EvaluatorFunc) Evaluate(ctx context.Context, evaluation Evaluation) (Verdict, error) {
	if function == nil {
		return Verdict{}, errors.New("nil goal evaluator")
	}
	return function(ctx, evaluation)
}

// SelfEvaluator accepts only a report emitted through goal_report during the
// current round. Prose claims in the final answer are intentionally not parsed
// as control signals.
type SelfEvaluator struct{}

func (SelfEvaluator) Evaluate(_ context.Context, evaluation Evaluation) (Verdict, error) {
	if evaluation.Report == nil {
		return Verdict{
			Decision: DecisionContinue,
			Reason:   "the acting Agent did not submit a goal_report",
			Feedback: "Assess the original goal explicitly and call goal_report before returning another candidate.",
		}, nil
	}
	verdict := Verdict{Decision: evaluation.Report.Decision, Reason: evaluation.Report.Reason}
	if verdict.Decision == DecisionContinue {
		verdict.Feedback = evaluation.Report.Reason
	}
	return verdict, verdict.Validate()
}

// HybridEvaluator requires independent agreement for completion or blockage.
// Disagreement is converted to another reasoning round with both assessments
// visible, which is safer than treating either signal as authoritative.
type HybridEvaluator struct {
	Self        Evaluator
	Independent Evaluator
}

func (evaluator HybridEvaluator) Evaluate(ctx context.Context, evaluation Evaluation) (Verdict, error) {
	if evaluator.Self == nil || evaluator.Independent == nil {
		return Verdict{}, errors.New("hybrid goal assessment requires self and independent evaluators")
	}
	self, err := evaluator.Self.Evaluate(ctx, evaluation)
	if err != nil {
		return Verdict{}, fmt.Errorf("self goal assessment: %w", err)
	}
	independent, err := evaluator.Independent.Evaluate(ctx, evaluation)
	if err != nil {
		return Verdict{}, fmt.Errorf("independent goal assessment: %w", err)
	}
	if self.Decision == independent.Decision && self.Decision != DecisionContinue {
		return Verdict{Decision: self.Decision, Reason: "self and independent assessments agree: " + independent.Reason}, nil
	}
	reason := fmt.Sprintf("assessment disagreement: self=%s (%s); independent=%s (%s)", self.Decision, self.Reason, independent.Decision, independent.Reason)
	return Verdict{Decision: DecisionContinue, Reason: reason, Feedback: reason}, nil
}

func Compose(strategy AssessmentStrategy, independent Evaluator) (Evaluator, error) {
	self := SelfEvaluator{}
	switch strategy {
	case AssessmentAdaptive:
		if independent == nil {
			return nil, errors.New("adaptive goal assessment requires a semantic fallback")
		}
		return AdaptiveEvaluator{Fallback: independent}, nil
	case AssessmentSelf:
		return self, nil
	case AssessmentIndependent:
		if independent == nil {
			return nil, errors.New("independent goal assessment is unavailable")
		}
		return independent, nil
	case AssessmentHybrid:
		if independent == nil {
			return nil, errors.New("hybrid goal assessment is unavailable")
		}
		return HybridEvaluator{Self: self, Independent: independent}, nil
	default:
		return nil, fmt.Errorf("unsupported goal assessment strategy %q", strategy)
	}
}

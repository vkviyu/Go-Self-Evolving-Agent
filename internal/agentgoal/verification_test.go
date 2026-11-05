package agentgoal

import (
	"context"
	"errors"
	"testing"
)

type verifierFunc func(context.Context, Evaluation) (Verification, error)

func (verify verifierFunc) Verify(ctx context.Context, evaluation Evaluation) (Verification, error) {
	return verify(ctx, evaluation)
}

func TestAdaptiveVerificationRequiresCompleteCoverageAndRespectsBlockers(t *testing.T) {
	for _, tc := range []struct {
		name           string
		complete, pass bool
		report         Decision
		want           Decision
		fallback       bool
	}{
		{"all checks", true, true, DecisionComplete, DecisionComplete, false},
		{"subset", false, true, DecisionComplete, DecisionComplete, true},
		{"failed check", true, false, DecisionComplete, DecisionContinue, false},
		{"external blocker", true, false, DecisionBlocked, DecisionBlocked, true},
		{"missing report", true, true, "", DecisionComplete, true},
		{"agent still working", true, true, DecisionContinue, DecisionComplete, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			evaluator := AdaptiveEvaluator{
				Verifier: verifierFunc(func(context.Context, Evaluation) (Verification, error) {
					return Verification{Complete: tc.complete, Satisfied: tc.pass, Evidence: []string{"trusted check"}}, nil
				}),
				Fallback: EvaluatorFunc(func(_ context.Context, e Evaluation) (Verdict, error) {
					calls++
					if len(e.Evidence) != 1 {
						t.Fatal("mechanical evidence lost")
					}
					return Verdict{Decision: tc.want, Reason: "semantic assessment"}, nil
				}),
			}
			evaluation := Evaluation{Goal: Goal{Contract: &Contract{}}}
			if tc.report != "" {
				evaluation.Report = &Report{Decision: tc.report}
			}
			verdict, err := evaluator.Evaluate(context.Background(), evaluation)
			if err != nil || verdict.Decision != tc.want || (calls > 0) != tc.fallback {
				t.Fatalf("verdict=%+v calls=%d err=%v", verdict, calls, err)
			}
		})
	}
}

func TestVerificationFailureIsNotAnExecutionRetry(t *testing.T) {
	failure := errors.New("permission denied")
	evaluator := AdaptiveEvaluator{Verifier: verifierFunc(func(context.Context, Evaluation) (Verification, error) { return Verification{}, failure })}
	verdict, err := evaluator.Evaluate(context.Background(), Evaluation{Goal: Goal{Contract: &Contract{}}})
	if !errors.Is(err, failure) || verdict.Decision != "" {
		t.Fatalf("verdict=%+v err=%v", verdict, err)
	}
}

func TestSemanticAssessmentCannotOverrideFailedCheck(t *testing.T) {
	evaluator := AdaptiveEvaluator{
		Verifier: verifierFunc(func(context.Context, Evaluation) (Verification, error) {
			return Verification{Complete: true, Satisfied: false}, nil
		}),
		Fallback: EvaluatorFunc(func(context.Context, Evaluation) (Verdict, error) {
			return Verdict{Decision: DecisionComplete, Reason: "model claims complete"}, nil
		}),
	}
	verdict, err := evaluator.Evaluate(context.Background(), Evaluation{Goal: Goal{Contract: &Contract{}}, Report: &Report{Decision: DecisionBlocked}})
	if err != nil || verdict.Decision == DecisionComplete {
		t.Fatalf("verdict=%+v err=%v", verdict, err)
	}
}

package agentgoal

import (
	"context"
	"strings"
	"testing"
)

func TestHybridEvaluatorRequiresAgreement(t *testing.T) {
	t.Parallel()
	evaluator := HybridEvaluator{
		Self: EvaluatorFunc(func(context.Context, Evaluation) (Verdict, error) {
			return Verdict{Decision: DecisionComplete, Reason: "acting Agent says complete"}, nil
		}),
		Independent: EvaluatorFunc(func(context.Context, Evaluation) (Verdict, error) {
			return Verdict{Decision: DecisionContinue, Reason: "tests are missing"}, nil
		}),
	}
	verdict, err := evaluator.Evaluate(context.Background(), Evaluation{})
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Decision != DecisionContinue || !strings.Contains(verdict.Reason, "disagreement") {
		t.Fatalf("verdict = %#v, want conservative continuation", verdict)
	}
}

func TestHybridEvaluatorAcceptsMatchingTerminalDecision(t *testing.T) {
	t.Parallel()
	complete := EvaluatorFunc(func(context.Context, Evaluation) (Verdict, error) {
		return Verdict{Decision: DecisionComplete, Reason: "verified"}, nil
	})
	verdict, err := (HybridEvaluator{Self: complete, Independent: complete}).Evaluate(context.Background(), Evaluation{})
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Decision != DecisionComplete {
		t.Fatalf("verdict = %#v", verdict)
	}
}

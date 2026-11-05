package agentgoal

import (
	"context"
	"errors"
	"strings"
)

// Verification describes the coverage and result of mechanically checked
// criteria. Passing a subset never proves the whole contract satisfied.
type Verification struct {
	Complete  bool
	Satisfied bool
	Evidence  []string
}

type Verifier interface {
	Verify(context.Context, Evaluation) (Verification, error)
}

// AdaptiveEvaluator keeps verification separate from semantic assessment.
// Infrastructure failures propagate; they are not requests to redo side effects.
type AdaptiveEvaluator struct {
	Verifier Verifier
	Fallback Evaluator
}

func (evaluator AdaptiveEvaluator) Evaluate(ctx context.Context, evaluation Evaluation) (Verdict, error) {
	mechanicalFailure := false
	if evaluator.Verifier != nil && evaluation.Goal.Contract != nil {
		checked, err := evaluator.Verifier.Verify(ctx, evaluation)
		if err != nil {
			return Verdict{}, err
		}
		evaluation.Evidence = append(evaluation.Evidence, checked.Evidence...)
		mechanicalFailure = !checked.Satisfied
		// A genuine blocker still needs semantic assessment, not an automatic
		// retry of a failed check. Never override an acting agent's blocker.
		blocked := evaluation.Report != nil && evaluation.Report.Decision == DecisionBlocked
		if !checked.Satisfied && !blocked {
			return Verdict{Decision: DecisionContinue, Reason: "acceptance checks failed", Feedback: joinEvidence(checked.Evidence)}, nil
		}
		if checked.Complete && checked.Satisfied && evaluation.Report != nil && evaluation.Report.Decision == DecisionComplete {
			return Verdict{Decision: DecisionComplete, Reason: "all declared acceptance criteria passed mechanical verification"}, nil
		}
	}
	if evaluator.Fallback == nil {
		return Verdict{}, errors.New("semantic goal assessment is unavailable")
	}
	verdict, err := evaluator.Fallback.Evaluate(ctx, evaluation)
	if err == nil && mechanicalFailure && verdict.Decision == DecisionComplete {
		return Verdict{Decision: DecisionContinue, Reason: "semantic assessment cannot override failed mechanical acceptance checks", Feedback: joinEvidence(evaluation.Evidence)}, nil
	}
	return verdict, err
}

func joinEvidence(evidence []string) string {
	var result strings.Builder
	result.WriteString("Address only the unmet criteria within the original authorized scope.")
	for _, item := range evidence {
		result.WriteString("\n")
		result.WriteString(item)
	}
	return result.String()
}

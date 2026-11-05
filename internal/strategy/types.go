// Package strategy describes bounded LoopSpec evolution. A spec is data, not
// executable code, and cannot change tools, models, permissions or Goal gates.
package strategy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type Spec struct {
	Version           int    `json:"version"`
	Revision          string `json:"revision"`
	OwnerSession      string `json:"owner_session,omitempty"`
	InstructionSuffix string `json:"instruction_suffix"`
	MaxIterations     int    `json:"max_iterations,omitempty"`
	MaxOutputTokens   int    `json:"max_output_tokens,omitempty"`
}

var ErrEvidence error = evidenceError{}

type evidenceError struct{}

func (evidenceError) Error() string {
	return "strategy evidence or publication outcome could not be committed"
}
func (evidenceError) ToolExecutionFatal() bool { return true }

func (spec Spec) Validate() error {
	if spec.Version != 1 || strings.TrimSpace(spec.Revision) == "" || len(spec.Revision) > 128 || len(spec.OwnerSession) > 128 || len(spec.InstructionSuffix) > 8192 || spec.MaxIterations < 0 || spec.MaxIterations > 100 || spec.MaxOutputTokens < 0 || spec.MaxOutputTokens > 32768 {
		return errors.New("invalid bounded LoopSpec")
	}
	if spec.InstructionSuffix == "" && spec.MaxIterations == 0 && spec.MaxOutputTokens == 0 {
		return errors.New("empty strategy candidate")
	}
	return nil
}

func Digest(value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func (spec Spec) ID() string { return Digest(spec) }

// Store is the execution-time port: only the composition root can publish.
type Store interface {
	Active(context.Context, string) (*Spec, error)
	Load(context.Context, string) (Spec, error)
	Fallback(context.Context, string, string, string) error
}

type Fixture struct {
	Name     string `json:"name"`
	Prompt   string `json:"prompt"`
	Expected string `json:"expected"`
	Safety   bool   `json:"safety"`
}

type Suite struct {
	Name     string    `json:"name"`
	Fixtures []Fixture `json:"fixtures"`
}

func (suite Suite) Validate() error {
	if suite.Name == "" || len(suite.Name) > 128 || len(suite.Fixtures) < 1 || len(suite.Fixtures) > 8 {
		return errors.New("shadow suite requires a bounded name and 1 to 8 fixtures")
	}
	seen := map[string]bool{}
	safety := false
	for _, fixture := range suite.Fixtures {
		if fixture.Name == "" || len(fixture.Name) > 128 || seen[fixture.Name] || fixture.Prompt == "" || len(fixture.Prompt) > 4096 || fixture.Expected == "" || len(fixture.Expected) > 4096 {
			return errors.New("invalid shadow fixture")
		}
		seen[fixture.Name] = true
		safety = safety || fixture.Safety
	}
	if !safety {
		return errors.New("shadow suite must include an explicit safety regression fixture")
	}
	return nil
}

type Outcome struct {
	Fixture     string `json:"fixture"`
	Passed      bool   `json:"passed"`
	Safety      bool   `json:"safety"`
	ModelCalls  int    `json:"model_calls"`
	OutputBytes int    `json:"output_bytes"`
	DurationMS  int64  `json:"duration_ms"`
	Error       string `json:"error,omitempty"`
}

type Report struct {
	CandidateID      string    `json:"candidate_id"`
	BaselineConfigID string    `json:"baseline_config_id"`
	PreviousSpecID   string    `json:"previous_spec_id,omitempty"`
	SuiteID          string    `json:"suite_id"`
	Baseline         []Outcome `json:"baseline"`
	Candidate        []Outcome `json:"candidate"`
	Eligible         bool      `json:"eligible"`
	Reason           string    `json:"reason"`
	Failure          string    `json:"failure,omitempty"`
	EvaluatedAt      time.Time `json:"evaluated_at"`
}

func (report Report) Acceptance() (bool, string) {
	if report.Failure != "" {
		return false, "comparison failed: " + report.Failure
	}
	if len(report.Baseline) == 0 || len(report.Baseline) != len(report.Candidate) {
		return false, "incomplete comparison"
	}
	basePassed, candidatePassed, baseCalls, candidateCalls := 0, 0, 0, 0
	safety := false
	seen := map[string]bool{}
	for index, baseline := range report.Baseline {
		candidate := report.Candidate[index]
		if baseline.Fixture == "" || seen[baseline.Fixture] || baseline.ModelCalls < 1 || candidate.ModelCalls < 1 || baseline.OutputBytes < 0 || candidate.OutputBytes < 0 || baseline.DurationMS < 0 || candidate.DurationMS < 0 || baseline.Fixture != candidate.Fixture || baseline.Safety != candidate.Safety || baseline.Error != "" || candidate.Error != "" {
			return false, "fixture identity mismatch or execution failure"
		}
		seen[baseline.Fixture] = true
		if baseline.Passed {
			basePassed++
		}
		if candidate.Passed {
			candidatePassed++
		}
		baseCalls += baseline.ModelCalls
		candidateCalls += candidate.ModelCalls
		if candidate.Safety {
			safety = true
			if !candidate.Passed {
				return false, "candidate failed a required safety fixture"
			}
		}
	}
	if !safety || candidatePassed < basePassed || candidateCalls > baseCalls {
		return false, "candidate regressed exact-match score or model-call count"
	}
	return true, "no regression on this explicit tool-free suite; not a claim of general capability improvement"
}

// ValidateSuite binds every scored outcome to the host's immutable expectations.
// Execution failures may be stored, but never qualify for promotion.
func (report Report) ValidateSuite(suite Suite) error {
	if err := suite.Validate(); err != nil {
		return err
	}
	if report.SuiteID != Digest(suite) || report.CandidateID == "" || report.EvaluatedAt.IsZero() || len(report.Failure) > 512 {
		return errors.New("shadow report does not identify its trusted comparison")
	}
	if report.Failure == "" && (report.BaselineConfigID == "" || len(report.Baseline) != len(suite.Fixtures) || len(report.Candidate) != len(suite.Fixtures)) {
		return errors.New("shadow report is not a complete comparison of its trusted suite")
	}
	for _, outcomes := range [][]Outcome{report.Baseline, report.Candidate} {
		if len(outcomes) > len(suite.Fixtures) {
			return errors.New("shadow report has unexpected fixture outcomes")
		}
		for i, outcome := range outcomes {
			fixture := suite.Fixtures[i]
			if outcome.Fixture != fixture.Name || outcome.Safety != fixture.Safety || outcome.ModelCalls < 0 || outcome.OutputBytes < 0 || outcome.DurationMS < 0 || len(outcome.Error) > 512 {
				return errors.New("shadow outcome does not match its trusted fixture")
			}
		}
	}
	return nil
}

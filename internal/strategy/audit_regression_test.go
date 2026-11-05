package strategy

import (
	"testing"
	"time"
)

func TestFailedComparisonEvidenceCannotBecomePromotion(t *testing.T) {
	suite := Suite{Name: "trusted", Fixtures: []Fixture{{Name: "safety", Prompt: "safe", Expected: "yes", Safety: true}}}
	report := Report{CandidateID: "candidate", SuiteID: Digest(suite), Failure: "provider interrupted", EvaluatedAt: time.Now().UTC()}
	if err := report.ValidateSuite(suite); err != nil {
		t.Fatalf("lost failed comparison evidence: %v", err)
	}
	if accepted, _ := report.Acceptance(); accepted {
		t.Fatal("failed comparison eligible for promotion")
	}
	report.Baseline = []Outcome{{Fixture: "foreign", Safety: true}}
	if err := report.ValidateSuite(suite); err == nil {
		t.Fatal("failed evidence accepted a foreign fixture")
	}
}

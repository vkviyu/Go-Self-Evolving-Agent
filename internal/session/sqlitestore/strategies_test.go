package sqlitestore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/strategy"
)

func strategyTestReport(t *testing.T, db *Database, revision string) (strategy.Spec, strategy.Report, string) {
	t.Helper()
	ctx := context.Background()
	store := db.Strategies()
	spec := strategy.Spec{Version: 1, OwnerSession: "owner", Revision: revision, InstructionSuffix: "Use explicit evidence"}
	id, err := store.Save(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	suite := strategy.Suite{Name: "trusted", Fixtures: []strategy.Fixture{{Name: "safety", Prompt: "Do not call tools", Expected: "OK", Safety: true}, {Name: "accuracy", Prompt: "1+1", Expected: "2"}}}
	suiteID, err := store.SaveSuite(ctx, suite, true)
	if err != nil {
		t.Fatal(err)
	}
	results := []strategy.Outcome{{Fixture: "safety", Safety: true, Passed: true, ModelCalls: 1}, {Fixture: "accuracy", Passed: true, ModelCalls: 1}}
	report := strategy.Report{CandidateID: id, BaselineConfigID: "base", SuiteID: suiteID, EvaluatedAt: time.Now().UTC(), Baseline: append([]strategy.Outcome(nil), results...), Candidate: append([]strategy.Outcome(nil), results...)}
	reportID, err := store.SaveReport(ctx, report)
	if err != nil {
		t.Fatal(err)
	}
	return spec, report, reportID
}

func TestStrategyImmutablePromotionCASAndFallback(t *testing.T) {
	ctx := context.Background()
	db := abandonmentDatabase(t)
	store := db.Strategies()
	spec, _, id := strategyTestReport(t, db, "v1")
	if err := store.Promote(ctx, id, false); err == nil {
		t.Fatal("published without consent")
	}
	if err := store.Promote(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if err := store.Promote(ctx, id, true); err != nil {
		t.Fatal("idempotent publish", err)
	}
	active, err := store.Active(ctx, "base")
	if err != nil || active == nil || active.ID() != spec.ID() {
		t.Fatalf("active=%+v err=%v", active, err)
	}
	if other, err := store.Active(ctx, "different-config"); err != nil || other != nil {
		t.Fatal("activation leaked across baseline")
	}
	changed := spec
	changed.InstructionSuffix = "rewritten"
	if _, err := store.Save(ctx, changed); err == nil {
		t.Fatal("rebound immutable revision")
	}
	_, report, stale := strategyTestReport(t, db, "v2")
	if err := store.Promote(ctx, stale, true); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatal("accepted stale baseline", err)
	}
	report.PreviousSpecID = spec.ID()
	fresh, err := store.SaveReport(ctx, report)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Promote(ctx, fresh, true); err != nil {
		t.Fatal(err)
	}
	if err := store.Fallback(ctx, "base", spec.ID(), "old failure"); err != nil {
		t.Fatal(err)
	}
	active, _ = store.Active(ctx, "base")
	if active == nil || active.ID() != report.CandidateID {
		t.Fatal("old failure removed newer strategy")
	}
	if err := store.Fallback(ctx, "base", report.CandidateID, "failed turn"); err != nil {
		t.Fatal(err)
	}
	active, _ = store.Active(ctx, "base")
	if active != nil {
		t.Fatal("fallback did not restore base")
	}
	if err := store.Promote(ctx, fresh, true); err == nil {
		t.Fatal("withdrawn candidate republished from old evidence")
	}
	var events int
	if err := db.db.QueryRow(`SELECT count(*) FROM strategy_events`).Scan(&events); err != nil || events != 3 {
		t.Fatalf("audit=%d %v", events, err)
	}
}

func TestStrategyReportGateRejectsMissingSafetyTamperAndAuditFailure(t *testing.T) {
	for _, mode := range []string{"safety", "regression", "cost", "zero-calls", "duplicate", "missing", "wrong-suite", "wrong-safety", "tamper", "audit-failure"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			db := abandonmentDatabase(t)
			store := db.Strategies()
			_, report, id := strategyTestReport(t, db, "v1")
			switch mode {
			case "safety":
				report.Candidate[0].Passed = false
			case "regression":
				report.Candidate[1].Passed = false
			case "cost":
				report.Candidate[1].ModelCalls = 2
			case "zero-calls":
				report.Candidate[0].ModelCalls = 0
			case "duplicate":
				report.Candidate[1] = report.Candidate[0]
				report.Baseline[1] = report.Baseline[0]
			case "missing":
				report.Candidate = report.Candidate[:1]
				report.Baseline = report.Baseline[:1]
			case "wrong-suite":
				report.SuiteID = "unknown"
			case "wrong-safety":
				report.Candidate[0].Safety = false
				report.Baseline[0].Safety = false
			case "tamper":
				if _, err := db.db.Exec(`UPDATE strategy_reports SET report='{}' WHERE report_id=?`, id); err != nil {
					t.Fatal(err)
				}
			case "audit-failure":
				if _, err := db.db.Exec(`CREATE TRIGGER fail_strategy BEFORE INSERT ON strategy_events BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
					t.Fatal(err)
				}
			}
			if mode != "tamper" && mode != "audit-failure" {
				var err error
				id, err = store.SaveReport(ctx, report)
				if err != nil {
					return
				}
			}
			if err := store.Promote(ctx, id, true); err == nil {
				t.Fatal("unsafe report published")
			}
			if active, err := store.Active(ctx, "base"); err != nil || active != nil {
				t.Fatal("failed publish changed active pointer", err)
			}
		})
	}
}

func TestStrategySuiteAndCandidateChecksumsAndConsent(t *testing.T) {
	ctx := context.Background()
	db := abandonmentDatabase(t)
	store := db.Strategies()
	spec, report, _ := strategyTestReport(t, db, "v1")
	suite, _ := store.Suite(ctx, report.SuiteID)
	if _, err := store.SaveSuite(ctx, suite, false); err == nil {
		t.Fatal("registered expectations without confirmation")
	}
	if _, err := db.db.Exec(`UPDATE strategy_specs SET spec=spec || ' ' WHERE spec_id=?`, spec.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(ctx, spec); err == nil {
		t.Fatal("idempotent save masked corruption")
	}
	if _, err := db.db.Exec(`UPDATE strategy_suites SET suite=suite || ' ' WHERE suite_id=?`, report.SuiteID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveSuite(ctx, suite, true); err == nil {
		t.Fatal("suite save masked corruption")
	}
}

func TestStrategyActiveRechecksPublicationEvidence(t *testing.T) {
	ctx := context.Background()
	db := abandonmentDatabase(t)
	store := db.Strategies()
	_, _, reportID := strategyTestReport(t, db, "active")
	if err := store.Promote(ctx, reportID, true); err != nil {
		t.Fatal(err)
	}
	other, _, _ := strategyTestReport(t, db, "unpublished")
	if _, err := db.db.Exec(`UPDATE strategy_active SET spec_id=? WHERE baseline_id='base'`, other.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Active(ctx, "base"); err == nil {
		t.Fatal("active pointer tampering bypassed publication gate")
	}
}

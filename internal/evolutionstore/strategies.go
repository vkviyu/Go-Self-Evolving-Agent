package evolutionstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/strategy"
)

func (s *Store) Save(ctx context.Context, spec strategy.Spec) (string, error) {
	if err := spec.Validate(); err != nil {
		return "", err
	}
	doc, err := encode(spec)
	if err != nil {
		return "", err
	}
	err = s.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		key := recordKey("strategy-revision", spec.OwnerSession, spec.Revision)
		var previous string
		found, err := tx.Get(key, &previous)
		if err != nil {
			return err
		}
		if found && previous != doc.Digest {
			return fmt.Errorf("%w: immutable strategy revision", orchestration.ErrIdempotencyConflict)
		}
		if err := saveDocument(tx, "strategy-spec", doc); err != nil {
			return err
		}
		if !found {
			return tx.Create(key, doc.Digest)
		}
		return nil
	})
	return doc.Digest, err
}

func loadSpec(tx *ateauthority.Tx, id string) (strategy.Spec, error) {
	var spec strategy.Spec
	if err := loadDocument(tx, "strategy-spec", id, &spec); err != nil {
		return spec, err
	}
	return spec, spec.Validate()
}

func (s *Store) Load(ctx context.Context, id string) (spec strategy.Spec, err error) {
	err = s.authority.View(ctx, func(tx *ateauthority.Tx) error {
		spec, err = loadSpec(tx, id)
		return err
	})
	return
}

type activation struct {
	SpecID   string `json:"spec_id"`
	ReportID string `json:"report_id"`
}

func (s *Store) Active(ctx context.Context, baseline string) (spec *strategy.Spec, err error) {
	err = s.authority.View(ctx, func(tx *ateauthority.Tx) error {
		var active activation
		found, err := tx.Get(recordKey("strategy-active", baseline), &active)
		if err != nil || !found || (active.SpecID == "" && active.ReportID == "") {
			return err
		}
		report, err := loadReport(tx, active.ReportID)
		if err != nil {
			return err
		}
		eligible, _ := report.Acceptance()
		if report.CandidateID != active.SpecID || report.BaselineConfigID != baseline || !eligible || !report.Eligible {
			return errors.New("active strategy does not match its passing publication evidence")
		}
		var reason string
		withdrawn, err := tx.Get(recordKey("strategy-quarantine", baseline, active.SpecID), &reason)
		if err != nil {
			return err
		}
		if withdrawn {
			return errors.New("active strategy is quarantined")
		}
		value, err := loadSpec(tx, active.SpecID)
		if err == nil {
			spec = &value
		}
		return err
	})
	return
}

func (s *Store) SaveSuite(ctx context.Context, suite strategy.Suite, confirmed bool) (string, error) {
	if !confirmed {
		return "", errors.New("registering trusted shadow expectations requires explicit confirmation")
	}
	if err := suite.Validate(); err != nil {
		return "", err
	}
	doc, err := encode(suite)
	if err != nil {
		return "", err
	}
	err = s.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		return saveDocument(tx, "strategy-suite", doc)
	})
	return doc.Digest, err
}

func loadSuite(tx *ateauthority.Tx, id string) (strategy.Suite, error) {
	var suite strategy.Suite
	if err := loadDocument(tx, "strategy-suite", id, &suite); err != nil {
		return suite, err
	}
	return suite, suite.Validate()
}

func (s *Store) Suite(ctx context.Context, id string) (suite strategy.Suite, err error) {
	err = s.authority.View(ctx, func(tx *ateauthority.Tx) error {
		suite, err = loadSuite(tx, id)
		return err
	})
	return
}

func (s *Store) SaveReport(ctx context.Context, report strategy.Report) (string, error) {
	report.Eligible, report.Reason = report.Acceptance()
	doc, err := encode(report)
	if err != nil {
		return "", err
	}
	err = s.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		if _, err := loadSpec(tx, report.CandidateID); err != nil {
			return err
		}
		suite, err := loadSuite(tx, report.SuiteID)
		if err != nil {
			return err
		}
		if err := report.ValidateSuite(suite); err != nil {
			return err
		}
		if report.PreviousSpecID != "" {
			if _, err := loadSpec(tx, report.PreviousSpecID); err != nil {
				return err
			}
		}
		return saveDocument(tx, "strategy-report", doc)
	})
	return doc.Digest, err
}

func loadReport(tx *ateauthority.Tx, id string) (strategy.Report, error) {
	var report strategy.Report
	if err := loadDocument(tx, "strategy-report", id, &report); err != nil {
		return report, err
	}
	suite, err := loadSuite(tx, report.SuiteID)
	if err != nil {
		return report, err
	}
	return report, report.ValidateSuite(suite)
}

func (s *Store) Report(ctx context.Context, id string) (report strategy.Report, err error) {
	err = s.authority.View(ctx, func(tx *ateauthority.Tx) error {
		report, err = loadReport(tx, id)
		return err
	})
	return
}

// Promote validates evidence and swaps the tested baseline at one pinned root.
// The activation and its event are committed by the same native ATE mutation.
func (s *Store) Promote(ctx context.Context, reportID string, confirmed bool) error {
	if !confirmed {
		return errors.New("strategy promotion requires explicit confirmation")
	}
	return publicationError(s.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		report, err := loadReport(tx, reportID)
		if err != nil {
			return err
		}
		eligible, _ := report.Acceptance()
		if !eligible || !report.Eligible || report.BaselineConfigID == "" {
			return errors.New("strategy report does not pass the promotion gate")
		}
		if _, err := loadSpec(tx, report.CandidateID); err != nil {
			return err
		}
		var reason string
		withdrawn, err := tx.Get(recordKey("strategy-quarantine", report.BaselineConfigID, report.CandidateID), &reason)
		if err != nil {
			return err
		}
		if withdrawn {
			return errors.New("withdrawn strategy requires a new immutable revision and fresh comparison")
		}
		key := recordKey("strategy-active", report.BaselineConfigID)
		var previous activation
		if _, err := tx.Get(key, &previous); err != nil {
			return err
		}
		if previous.SpecID == report.CandidateID && previous.ReportID == reportID {
			return nil
		}
		if previous.SpecID != report.PreviousSpecID {
			return orchestration.ErrIdempotencyConflict
		}
		if err := tx.Put(key, activation{SpecID: report.CandidateID, ReportID: reportID}); err != nil {
			return err
		}
		return appendStrategyEvent(tx, "strategy.promoted", map[string]string{
			"report_id": reportID, "baseline_id": report.BaselineConfigID, "spec_id": report.CandidateID,
		})
	}))
}

func (s *Store) Fallback(ctx context.Context, baseline, id, reason string) error {
	if baseline == "" || id == "" {
		return errors.New("fallback requires exact baseline and candidate")
	}
	return publicationError(s.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		key := recordKey("strategy-active", baseline)
		var active activation
		found, err := tx.Get(key, &active)
		if err != nil || !found || active.SpecID != id {
			return err
		}
		if err := tx.Put(key, activation{}); err != nil {
			return err
		}
		quarantine := recordKey("strategy-quarantine", baseline, id)
		var previous string
		withdrawn, err := tx.Get(quarantine, &previous)
		if err != nil {
			return err
		}
		if !withdrawn {
			if err := tx.Create(quarantine, reason); err != nil {
				return err
			}
		}
		return appendStrategyEvent(tx, "strategy.fallback", map[string]string{
			"baseline_id": baseline, "spec_id": id, "reason": reason,
		})
	}))
}

type strategyEvent struct {
	Sequence   uint64            `json:"sequence"`
	Type       string            `json:"event_type"`
	Payload    map[string]string `json:"payload"`
	Checksum   string            `json:"checksum"`
	OccurredAt time.Time         `json:"occurred_at"`
}

func appendStrategyEvent(tx *ateauthority.Tx, kind string, payload map[string]string) error {
	var sequence uint64
	counter := recordKey("strategy-event-sequence")
	if _, err := tx.Get(counter, &sequence); err != nil {
		return err
	}
	if sequence == ^uint64(0) {
		return errors.New("strategy event sequence exhausted")
	}
	sequence++
	event := strategyEvent{Sequence: sequence, Type: kind, Payload: payload,
		Checksum: strategy.Digest(payload), OccurredAt: time.Now().UTC()}
	// Decimal sequence keys preserve numeric order in native prefix scans.
	key := fmt.Sprintf("%s/%020d", recordKey("strategy-event"), sequence)
	if err := tx.Create(key, event); err != nil {
		return err
	}
	return tx.Put(counter, sequence)
}

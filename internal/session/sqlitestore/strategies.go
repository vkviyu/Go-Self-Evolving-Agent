package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/retention"
	"github.com/vkviyu/go-self-evolving-agent/internal/strategy"
)

type Strategies struct{ db *sql.DB }

var (
	_ strategy.Catalog     = (*Strategies)(nil)
	_ strategy.ShadowStore = (*Strategies)(nil)
)

func (database *Database) Strategies() *Strategies { return &Strategies{db: database.db} }

func (store *Strategies) Save(ctx context.Context, spec strategy.Spec) (string, error) {
	if err := spec.Validate(); err != nil {
		return "", err
	}
	id := spec.ID()
	raw, _ := json.Marshal(spec)
	_, err := store.db.ExecContext(ctx, `INSERT INTO strategy_specs(spec_id,owner_session,revision,spec) VALUES(?,?,?,?) ON CONFLICT(spec_id) DO NOTHING`, id, spec.OwnerSession, spec.Revision, raw)
	if err == nil {
		_, err = store.Load(ctx, id)
	}
	return id, err
}

func (store *Strategies) Load(ctx context.Context, id string) (strategy.Spec, error) {
	var spec strategy.Spec
	var raw []byte
	if err := store.db.QueryRowContext(ctx, `SELECT spec FROM strategy_specs WHERE spec_id=?`, id).Scan(&raw); err != nil {
		return spec, recordError(err, "strategy", id)
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return spec, err
	}
	if retention.Digest(raw) != id {
		return spec, errors.New("LoopSpec checksum mismatch")
	}
	return spec, spec.Validate()
}

func (store *Strategies) Active(ctx context.Context, baseline string) (*strategy.Spec, error) {
	var id, reportID string
	err := store.db.QueryRowContext(ctx, `SELECT spec_id,report_id FROM strategy_active WHERE baseline_id=?`, baseline).Scan(&id, &reportID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && id == "" && reportID == "") {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	report, err := store.Report(ctx, reportID)
	if err != nil {
		return nil, err
	}
	eligible, _ := report.Acceptance()
	if report.CandidateID != id || report.BaselineConfigID != baseline || !eligible || !report.Eligible {
		return nil, errors.New("active strategy does not match its passing publication evidence")
	}
	var withdrawn int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM strategy_quarantine WHERE baseline_id=? AND spec_id=?`, baseline, id).Scan(&withdrawn); err != nil {
		return nil, err
	}
	if withdrawn != 0 {
		return nil, errors.New("active strategy is quarantined")
	}
	spec, err := store.Load(ctx, id)
	return &spec, err
}

func (store *Strategies) SaveSuite(ctx context.Context, suite strategy.Suite, confirmed bool) (string, error) {
	if !confirmed {
		return "", errors.New("registering trusted shadow expectations requires explicit confirmation")
	}
	if err := suite.Validate(); err != nil {
		return "", err
	}
	id := strategy.Digest(suite)
	raw, _ := json.Marshal(suite)
	_, err := store.db.ExecContext(ctx, `INSERT INTO strategy_suites(suite_id,suite) VALUES(?,?) ON CONFLICT(suite_id) DO NOTHING`, id, raw)
	if err == nil {
		_, err = store.Suite(ctx, id)
	}
	return id, err
}

func (store *Strategies) Suite(ctx context.Context, id string) (strategy.Suite, error) {
	var suite strategy.Suite
	var raw []byte
	if err := store.db.QueryRowContext(ctx, `SELECT suite FROM strategy_suites WHERE suite_id=?`, id).Scan(&raw); err != nil {
		return suite, recordError(err, "strategy suite", id)
	}
	if err := json.Unmarshal(raw, &suite); err != nil {
		return suite, err
	}
	if retention.Digest(raw) != id {
		return suite, errors.New("suite checksum mismatch")
	}
	return suite, suite.Validate()
}

func (store *Strategies) SaveReport(ctx context.Context, report strategy.Report) (string, error) {
	eligible, reason := report.Acceptance()
	report.Eligible, report.Reason = eligible, reason
	if _, err := store.Load(ctx, report.CandidateID); err != nil {
		return "", err
	}
	suite, err := store.Suite(ctx, report.SuiteID)
	if err != nil {
		return "", err
	}
	if err := report.ValidateSuite(suite); err != nil {
		return "", err
	}
	if report.PreviousSpecID != "" {
		if _, err := store.Load(ctx, report.PreviousSpecID); err != nil {
			return "", err
		}
	}
	id := strategy.Digest(report)
	raw, _ := json.Marshal(report)
	_, err = store.db.ExecContext(ctx, `INSERT INTO strategy_reports(report_id,report) VALUES(?,?) ON CONFLICT(report_id) DO NOTHING`, id, raw)
	if err == nil {
		_, err = store.Report(ctx, id)
	}
	return id, err
}

func (store *Strategies) Report(ctx context.Context, id string) (strategy.Report, error) {
	var report strategy.Report
	var raw []byte
	if err := store.db.QueryRowContext(ctx, `SELECT report FROM strategy_reports WHERE report_id=?`, id).Scan(&raw); err != nil {
		return report, recordError(err, "strategy report", id)
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		return report, err
	}
	if retention.Digest(raw) != id {
		return report, errors.New("shadow report checksum mismatch")
	}
	suite, err := store.Suite(ctx, report.SuiteID)
	if err != nil {
		return report, err
	}
	return report, report.ValidateSuite(suite)
}

// Promote rechecks score and compare-and-swaps the exact tested baseline.
func (store *Strategies) Promote(ctx context.Context, reportID string, confirmed bool) error {
	if !confirmed {
		return errors.New("strategy promotion requires explicit confirmation")
	}
	report, err := store.Report(ctx, reportID)
	if err != nil {
		return err
	}
	eligible, _ := report.Acceptance()
	if !eligible || !report.Eligible || report.BaselineConfigID == "" {
		return errors.New("strategy report does not pass the promotion gate")
	}
	if _, err := store.Load(ctx, report.CandidateID); err != nil {
		return err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var withdrawn int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM strategy_quarantine WHERE baseline_id=? AND spec_id=?`, report.BaselineConfigID, report.CandidateID).Scan(&withdrawn); err != nil {
		return err
	}
	if withdrawn != 0 {
		return errors.New("withdrawn strategy requires a new immutable revision and fresh comparison")
	}
	var previous, previousReport string
	err = tx.QueryRowContext(ctx, `SELECT spec_id,report_id FROM strategy_active WHERE baseline_id=?`, report.BaselineConfigID).Scan(&previous, &previousReport)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if previous == report.CandidateID && previousReport == reportID {
		return nil
	}
	if previous != report.PreviousSpecID {
		return orchestration.ErrIdempotencyConflict
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO strategy_active(baseline_id,spec_id,report_id) VALUES(?,?,?) ON CONFLICT(baseline_id) DO UPDATE SET spec_id=excluded.spec_id,report_id=excluded.report_id`, report.BaselineConfigID, report.CandidateID, reportID); err != nil {
		return err
	}
	if err := appendStrategyEvent(ctx, tx, "strategy.promoted", map[string]any{"report_id": reportID, "baseline_id": report.BaselineConfigID, "spec_id": report.CandidateID}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return errors.Join(strategy.ErrEvidence, err)
	}
	return nil
}

func (store *Strategies) Fallback(ctx context.Context, baseline, id, reason string) error {
	if baseline == "" || id == "" {
		return errors.New("fallback requires exact baseline and candidate")
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	updated, err := tx.ExecContext(ctx, `UPDATE strategy_active SET spec_id='',report_id='' WHERE baseline_id=? AND spec_id=?`, baseline, id)
	if err != nil {
		return err
	}
	count, err := updated.RowsAffected()
	if err != nil {
		return err
	}
	if count > 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO strategy_quarantine(baseline_id,spec_id,reason) VALUES(?,?,?) ON CONFLICT DO NOTHING`, baseline, id, reason); err != nil {
			return err
		}
		if err := appendStrategyEvent(ctx, tx, "strategy.fallback", map[string]any{"baseline_id": baseline, "spec_id": id, "reason": reason}); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return errors.Join(strategy.ErrEvidence, err)
	}
	return nil
}

func appendStrategyEvent(ctx context.Context, tx *sql.Tx, kind string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO strategy_events(event_type,payload,checksum,occurred_at) VALUES(?,?,?,?)`, kind, raw, strategy.Digest(value), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

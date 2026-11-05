package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

func (store *OrchestrationStore) CreateGoal(ctx context.Context, request agentgoal.CreateRequest) (agentgoal.Goal, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := request.Validate(); err != nil {
		return agentgoal.Goal{}, err
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return agentgoal.Goal{}, fmt.Errorf("begin goal creation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	goal, err := createGoal(ctx, tx, request)
	if err != nil {
		return agentgoal.Goal{}, err
	}
	if err := tx.Commit(); err != nil {
		return agentgoal.Goal{}, err
	}
	return goal, nil
}

func createGoal(ctx context.Context, tx *sql.Tx, request agentgoal.CreateRequest) (agentgoal.Goal, error) {
	if existing, loadErr := goalByID(ctx, tx, request.ID); loadErr == nil {
		if existing.SessionID != request.SessionID || existing.TeamID != request.TeamID || existing.AgentID != request.AgentID || existing.Text != request.Text || existing.Assessment != request.Assessment || existing.MaxRounds != request.MaxRounds || existing.MaxWallTime != request.MaxWallTime || !reflect.DeepEqual(existing.Contract, request.Contract) {
			return agentgoal.Goal{}, fmt.Errorf("%w: goal %s", orchestration.ErrIdempotencyConflict, request.ID)
		}
		return existing, nil
	} else if !errors.Is(loadErr, orchestration.ErrNotFound) {
		return agentgoal.Goal{}, loadErr
	}
	now := time.Now().UTC()
	started := request.StartedAt
	if started.IsZero() {
		started = now
	}
	contract, err := json.Marshal(request.Contract)
	if err != nil {
		return agentgoal.Goal{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO goal_runs(goal_id, session_id, team_id, agent_id, goal_text, state, assessment, max_rounds, max_wall_time_ns, version, started_at, updated_at, contract) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?)`,
		request.ID, string(request.SessionID), string(request.TeamID), string(request.AgentID), request.Text, string(agentgoal.StateRunning), string(request.Assessment), request.MaxRounds, request.MaxWallTime.Nanoseconds(), started.UTC().Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), contract); err != nil {
		return agentgoal.Goal{}, fmt.Errorf("insert goal: %w", err)
	}
	return goalByID(ctx, tx, request.ID)
}

func (store *OrchestrationStore) GetGoal(ctx context.Context, goalID string) (agentgoal.Goal, error) {
	return goalByID(ctx, store.db, goalID)
}

func (store *OrchestrationStore) GoalForTurn(ctx context.Context, sessionID session.ID, turnID string) (agentgoal.Goal, agentgoal.Round, bool, error) {
	var goalID string
	var number int
	err := store.db.QueryRowContext(ctx, `SELECT r.goal_id, r.round_number FROM goal_rounds r JOIN goal_runs g ON g.goal_id = r.goal_id WHERE r.turn_id = ? AND g.session_id = ?`, turnID, string(sessionID)).Scan(&goalID, &number)
	if errors.Is(err, sql.ErrNoRows) {
		return agentgoal.Goal{}, agentgoal.Round{}, false, nil
	}
	if err != nil {
		return agentgoal.Goal{}, agentgoal.Round{}, false, err
	}
	goal, err := goalByID(ctx, store.db, goalID)
	if err != nil {
		return agentgoal.Goal{}, agentgoal.Round{}, false, err
	}
	round, err := roundByNumber(ctx, store.db, goalID, number)
	if err != nil {
		return agentgoal.Goal{}, agentgoal.Round{}, false, err
	}
	return goal, round, true, nil
}

func (store *OrchestrationStore) StartRound(ctx context.Context, goalID string, number int, turnID, input string) (agentgoal.Round, error) {
	if goalID == "" || number <= 0 || turnID == "" {
		return agentgoal.Round{}, errors.New("goal round requires goal ID, positive number and turn ID")
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return agentgoal.Round{}, fmt.Errorf("begin goal round: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	round, err := startGoalRound(ctx, tx, goalID, number, turnID, input)
	if err != nil {
		return agentgoal.Round{}, err
	}
	if err := tx.Commit(); err != nil {
		return agentgoal.Round{}, err
	}
	return round, nil
}

func startGoalRound(ctx context.Context, tx *sql.Tx, goalID string, number int, turnID, input string) (agentgoal.Round, error) {
	goal, err := goalByID(ctx, tx, goalID)
	if err != nil {
		return agentgoal.Round{}, err
	}
	if goal.State != agentgoal.StateRunning {
		return agentgoal.Round{}, fmt.Errorf("goal %s is not running", goalID)
	}
	if existing, loadErr := roundByNumber(ctx, tx, goalID, number); loadErr == nil {
		if existing.TurnID != turnID || existing.Input != input {
			return agentgoal.Round{}, fmt.Errorf("%w: goal round %s/%d", orchestration.ErrIdempotencyConflict, goalID, number)
		}
		return existing, nil
	} else if !errors.Is(loadErr, orchestration.ErrNotFound) {
		return agentgoal.Round{}, loadErr
	}
	if number != goal.Round+1 || number > goal.MaxRounds {
		return agentgoal.Round{}, fmt.Errorf("goal round %d is not the next admissible round after %d", number, goal.Round)
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO goal_rounds(goal_id, round_number, turn_id, input, started_at) VALUES(?, ?, ?, ?, ?)`, goalID, number, turnID, input, now.Format(time.RFC3339Nano)); err != nil {
		return agentgoal.Round{}, fmt.Errorf("insert goal round: %w", err)
	}
	if err := execCAS(ctx, tx, orchestration.ErrSequenceConflict, `UPDATE goal_runs SET current_round = ?, version = version + 1, updated_at = ? WHERE goal_id = ? AND version = ?`, number, now.Format(time.RFC3339Nano), goalID, goal.Version); err != nil {
		return agentgoal.Round{}, fmt.Errorf("advance goal round: %w", err)
	}
	return roundByNumber(ctx, tx, goalID, number)
}

// AdmitGoal has no intermediate orphan-goal state, including on process failure.
func (store *OrchestrationStore) AdmitGoal(ctx context.Context, request agentgoal.CreateRequest, turnID string) (agentgoal.Goal, agentgoal.Round, error) {
	if err := request.Validate(); err != nil {
		return agentgoal.Goal{}, agentgoal.Round{}, err
	}
	if request.Contract == nil || turnID == "" {
		return agentgoal.Goal{}, agentgoal.Round{}, errors.New("admission requires a contract and turn ID")
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return agentgoal.Goal{}, agentgoal.Round{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := createGoal(ctx, tx, request); err != nil {
		return agentgoal.Goal{}, agentgoal.Round{}, err
	}
	round, err := startGoalRound(ctx, tx, request.ID, 1, turnID, request.Text)
	if err != nil {
		return agentgoal.Goal{}, agentgoal.Round{}, err
	}
	goal, err := goalByID(ctx, tx, request.ID)
	if err != nil {
		return agentgoal.Goal{}, agentgoal.Round{}, err
	}
	if err := tx.Commit(); err != nil {
		return agentgoal.Goal{}, agentgoal.Round{}, err
	}
	return goal, round, nil
}

func (store *OrchestrationStore) FinishRound(ctx context.Context, goalID string, number int, verdict agentgoal.Verdict, candidate string) (agentgoal.Round, error) {
	if err := verdict.Validate(); err != nil {
		return agentgoal.Round{}, err
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return agentgoal.Round{}, fmt.Errorf("begin goal round completion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	round, err := roundByNumber(ctx, tx, goalID, number)
	if err != nil {
		return agentgoal.Round{}, err
	}
	if round.CompletedAt != nil {
		if round.Decision != verdict.Decision || round.Reason != verdict.Reason || round.Feedback != verdict.Feedback || round.Candidate != candidate {
			return agentgoal.Round{}, fmt.Errorf("%w: completed goal round %s/%d", orchestration.ErrIdempotencyConflict, goalID, number)
		}
		if err := tx.Commit(); err != nil {
			return agentgoal.Round{}, err
		}
		return round, nil
	}
	goal, err := goalByID(ctx, tx, goalID)
	if err != nil {
		return agentgoal.Round{}, err
	}
	if goal.State != agentgoal.StateRunning || goal.Round != number {
		return agentgoal.Round{}, errors.New("cannot assess a stopped goal or stale round")
	}
	now := time.Now().UTC()
	if err := execCAS(ctx, tx, orchestration.ErrSequenceConflict, `UPDATE goal_rounds SET candidate = ?, decision = ?, reason = ?, feedback = ?, completed_at = ? WHERE goal_id = ? AND round_number = ? AND completed_at IS NULL`, candidate, string(verdict.Decision), verdict.Reason, verdict.Feedback, now.Format(time.RFC3339Nano), goalID, number); err != nil {
		return agentgoal.Round{}, fmt.Errorf("finish goal round: %w", err)
	}
	round, err = roundByNumber(ctx, tx, goalID, number)
	if err != nil {
		return agentgoal.Round{}, err
	}
	if err := tx.Commit(); err != nil {
		return agentgoal.Round{}, fmt.Errorf("commit goal round completion: %w", err)
	}
	return round, nil
}

func (store *OrchestrationStore) RecordReport(ctx context.Context, sessionID session.ID, turnID string, report agentgoal.Report) (agentgoal.Report, error) {
	if sessionID == "" || turnID == "" {
		return agentgoal.Report{}, errors.New("goal report requires session and turn IDs")
	}
	if err := report.Validate(); err != nil {
		return agentgoal.Report{}, err
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return agentgoal.Report{}, fmt.Errorf("begin goal report: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var goalID string
	var roundNumber int
	var goalState string
	if err := tx.QueryRowContext(ctx, `SELECT r.goal_id, r.round_number, g.state FROM goal_rounds r JOIN goal_runs g ON g.goal_id = r.goal_id WHERE r.turn_id = ? AND g.session_id = ?`, turnID, string(sessionID)).Scan(&goalID, &roundNumber, &goalState); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return agentgoal.Report{}, fmt.Errorf("%w: active goal round for turn %s", orchestration.ErrNotFound, turnID)
		}
		return agentgoal.Report{}, err
	}
	if agentgoal.State(goalState) != agentgoal.StateRunning {
		return agentgoal.Report{}, fmt.Errorf("goal %s is not running", goalID)
	}
	report.GoalID, report.Round, report.TurnID = goalID, roundNumber, turnID
	if existing, found, loadErr := reportForTurn(ctx, tx, sessionID, turnID); loadErr != nil {
		return agentgoal.Report{}, loadErr
	} else if found {
		if existing.Decision != report.Decision || existing.Reason != report.Reason || !slices.Equal(existing.Evidence, report.Evidence) || !slices.Equal(existing.Blockers, report.Blockers) {
			return agentgoal.Report{}, fmt.Errorf("%w: goal report for turn %s", orchestration.ErrIdempotencyConflict, turnID)
		}
		if err := tx.Commit(); err != nil {
			return agentgoal.Report{}, err
		}
		return existing, nil
	}
	evidence, _ := json.Marshal(report.Evidence)
	blockers, _ := json.Marshal(report.Blockers)
	report.CreatedAt = time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO goal_reports(turn_id, goal_id, round_number, decision, reason, evidence, blockers, created_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?)`, turnID, goalID, roundNumber, string(report.Decision), report.Reason, evidence, blockers, report.CreatedAt.Format(time.RFC3339Nano)); err != nil {
		return agentgoal.Report{}, fmt.Errorf("insert goal report: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return agentgoal.Report{}, fmt.Errorf("commit goal report: %w", err)
	}
	return report, nil
}

func (store *OrchestrationStore) ReportForTurn(ctx context.Context, sessionID session.ID, turnID string) (agentgoal.Report, bool, error) {
	return reportForTurn(ctx, store.db, sessionID, turnID)
}

func (store *OrchestrationStore) FinishGoal(ctx context.Context, goalID string, finish agentgoal.Finish) (agentgoal.Goal, error) {
	switch finish.State {
	case agentgoal.StateCompleted, agentgoal.StateBlocked, agentgoal.StateExhausted, agentgoal.StateFailed, agentgoal.StateCanceled:
	default:
		return agentgoal.Goal{}, fmt.Errorf("unsupported terminal goal state %q", finish.State)
	}
	if finish.Decision != "" {
		if err := finish.Decision.Validate(); err != nil {
			return agentgoal.Goal{}, err
		}
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return agentgoal.Goal{}, fmt.Errorf("begin goal completion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	goal, err := goalByID(ctx, tx, goalID)
	if err != nil {
		return agentgoal.Goal{}, err
	}
	if goal.State != agentgoal.StateRunning {
		if goal.State != finish.State || goal.Decision != finish.Decision || goal.StopReason != finish.StopReason || goal.Reason != finish.Reason || goal.Result != finish.Result {
			return agentgoal.Goal{}, fmt.Errorf("%w: terminal goal %s", orchestration.ErrIdempotencyConflict, goalID)
		}
		if err := tx.Commit(); err != nil {
			return agentgoal.Goal{}, err
		}
		return goal, nil
	}
	now := time.Now().UTC()
	if err := execCAS(ctx, tx, orchestration.ErrSequenceConflict, `UPDATE goal_runs SET state = ?, decision = ?, stop_reason = ?, reason = ?, result = ?, version = version + 1, updated_at = ?, completed_at = ? WHERE goal_id = ? AND version = ?`, string(finish.State), string(finish.Decision), string(finish.StopReason), finish.Reason, finish.Result, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), goalID, goal.Version); err != nil {
		return agentgoal.Goal{}, fmt.Errorf("finish goal: %w", err)
	}
	goal, err = goalByID(ctx, tx, goalID)
	if err != nil {
		return agentgoal.Goal{}, err
	}
	if err := tx.Commit(); err != nil {
		return agentgoal.Goal{}, fmt.Errorf("commit goal completion: %w", err)
	}
	return goal, nil
}

func (store *OrchestrationStore) CountGoals(ctx context.Context, sessionID session.ID) (int, error) {
	var count int
	err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM goal_runs WHERE session_id = ? AND state IN (?, ?, ?)`, string(sessionID), string(agentgoal.StateCompleted), string(agentgoal.StateBlocked), string(agentgoal.StateExhausted)).Scan(&count)
	return count, err
}

func goalByID(ctx context.Context, query rowQuery, goalID string) (agentgoal.Goal, error) {
	var goal agentgoal.Goal
	var sessionID, teamID, agentID, state, assessment, decision, stopReason, startedAt, updatedAt string
	var completedAt sql.NullString
	var wallTime int64
	var contract []byte
	err := query.QueryRowContext(ctx, `SELECT goal_id, session_id, team_id, agent_id, goal_text, state, assessment, current_round, max_rounds, max_wall_time_ns, decision, stop_reason, reason, result, version, started_at, updated_at, completed_at, contract FROM goal_runs WHERE goal_id = ?`, goalID).Scan(
		&goal.ID, &sessionID, &teamID, &agentID, &goal.Text, &state, &assessment, &goal.Round, &goal.MaxRounds, &wallTime, &decision, &stopReason, &goal.Reason, &goal.Result, &goal.Version, &startedAt, &updatedAt, &completedAt, &contract)
	if err != nil {
		return agentgoal.Goal{}, recordError(err, "goal", goalID)
	}
	if err := json.Unmarshal(contract, &goal.Contract); err != nil {
		return agentgoal.Goal{}, fmt.Errorf("decode task contract: %w", err)
	}
	goal.SessionID, goal.TeamID, goal.AgentID = session.ID(sessionID), orchestration.TeamID(teamID), orchestration.AgentID(agentID)
	goal.State, goal.Assessment, goal.Decision, goal.StopReason = agentgoal.State(state), agentgoal.AssessmentStrategy(assessment), agentgoal.Decision(decision), agentgoal.StopReason(stopReason)
	goal.MaxWallTime = time.Duration(wallTime)
	if goal.StartedAt, err = time.Parse(time.RFC3339Nano, startedAt); err != nil {
		return agentgoal.Goal{}, err
	}
	if goal.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return agentgoal.Goal{}, err
	}
	if completedAt.Valid {
		value, parseErr := time.Parse(time.RFC3339Nano, completedAt.String)
		if parseErr != nil {
			return agentgoal.Goal{}, parseErr
		}
		goal.CompletedAt = &value
	}
	return goal, nil
}

func roundByNumber(ctx context.Context, query rowQuery, goalID string, number int) (agentgoal.Round, error) {
	var round agentgoal.Round
	var startedAt string
	var completedAt sql.NullString
	var decision string
	err := query.QueryRowContext(ctx, `SELECT goal_id, round_number, turn_id, input, candidate, decision, reason, started_at, completed_at, feedback FROM goal_rounds WHERE goal_id = ? AND round_number = ?`, goalID, number).Scan(
		&round.GoalID, &round.Number, &round.TurnID, &round.Input, &round.Candidate, &decision, &round.Reason, &startedAt, &completedAt, &round.Feedback)
	if err != nil {
		return agentgoal.Round{}, recordError(err, "goal round", fmt.Sprintf("%s/%d", goalID, number))
	}
	round.Decision = agentgoal.Decision(decision)
	if round.StartedAt, err = time.Parse(time.RFC3339Nano, startedAt); err != nil {
		return agentgoal.Round{}, err
	}
	if completedAt.Valid {
		value, parseErr := time.Parse(time.RFC3339Nano, completedAt.String)
		if parseErr != nil {
			return agentgoal.Round{}, parseErr
		}
		round.CompletedAt = &value
	}
	return round, nil
}

func reportForTurn(ctx context.Context, query rowQuery, sessionID session.ID, turnID string) (agentgoal.Report, bool, error) {
	var report agentgoal.Report
	var decision, createdAt string
	var evidence, blockers []byte
	err := query.QueryRowContext(ctx, `SELECT p.goal_id, p.round_number, p.turn_id, p.decision, p.reason, p.evidence, p.blockers, p.created_at FROM goal_reports p JOIN goal_runs g ON g.goal_id = p.goal_id WHERE p.turn_id = ? AND g.session_id = ?`, turnID, string(sessionID)).Scan(
		&report.GoalID, &report.Round, &report.TurnID, &decision, &report.Reason, &evidence, &blockers, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return agentgoal.Report{}, false, nil
	}
	if err != nil {
		return agentgoal.Report{}, false, err
	}
	report.Decision = agentgoal.Decision(decision)
	if err := json.Unmarshal(evidence, &report.Evidence); err != nil {
		return agentgoal.Report{}, false, err
	}
	if err := json.Unmarshal(blockers, &report.Blockers); err != nil {
		return agentgoal.Report{}, false, err
	}
	report.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	return report, true, err
}

var _ agentgoal.Store = (*OrchestrationStore)(nil)

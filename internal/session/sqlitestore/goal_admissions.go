package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

const admissionColumns = `token, goal_id, session_id, team_id, agent_id, input_digest, contract_digest, config_digest, turn_id, round, state, version, reason, created_at, updated_at`

func scanAdmission(scanner rowScanner) (agentgoal.Admission, error) {
	var value agentgoal.Admission
	var teamID, agentID, state, createdAt, updatedAt string
	if err := scanner.Scan(&value.Token, &value.GoalID, &value.SessionID, &teamID, &agentID, &value.InputDigest, &value.ContractDigest, &value.ConfigDigest, &value.TurnID, &value.Round, &state, &value.Version, &value.Reason, &createdAt, &updatedAt); err != nil {
		return agentgoal.Admission{}, err
	}
	value.TeamID, value.AgentID, value.State = orchestration.TeamID(teamID), orchestration.AgentID(agentID), agentgoal.AdmissionState(state)
	var err error
	value.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return agentgoal.Admission{}, fmt.Errorf("parse Goal admission created_at: %w", err)
	}
	value.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return agentgoal.Admission{}, fmt.Errorf("parse Goal admission updated_at: %w", err)
	}
	if err := value.State.Validate(); err != nil {
		return agentgoal.Admission{}, err
	}
	return value, nil
}

func admissionByToken(ctx context.Context, query rowQuery, token string) (agentgoal.Admission, error) {
	return scanAdmission(query.QueryRowContext(ctx, `SELECT `+admissionColumns+` FROM goal_admissions WHERE token = ?`, token))
}

func (store *OrchestrationStore) Prepare(ctx context.Context, request agentgoal.AdmissionRequest) (agentgoal.Admission, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := request.Validate(); err != nil {
		return agentgoal.Admission{}, err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return agentgoal.Admission{}, fmt.Errorf("begin Goal admission: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if existing, lookupErr := admissionByToken(ctx, tx, request.Token); lookupErr == nil {
		if existing.GoalID != request.GoalID || existing.SessionID != request.SessionID || existing.TeamID != request.TeamID || existing.AgentID != request.AgentID || existing.InputDigest != request.InputDigest || existing.ContractDigest != request.ContractDigest || existing.ConfigDigest != request.ConfigDigest {
			return agentgoal.Admission{}, fmt.Errorf("%w: Goal admission token %q", orchestration.ErrIdempotencyConflict, request.Token)
		}
		if err := tx.Commit(); err != nil {
			return agentgoal.Admission{}, err
		}
		return existing, nil
	} else if !errors.Is(lookupErr, sql.ErrNoRows) {
		return agentgoal.Admission{}, fmt.Errorf("inspect Goal admission: %w", lookupErr)
	}
	// A Goal has one admission token for its entire lifetime. A retry that
	// lost the token in memory must recover the existing durable token rather
	// than hit a raw UNIQUE(goal_id) error or create a second capability.
	if existing, lookupErr := scanAdmission(tx.QueryRowContext(ctx, `SELECT `+admissionColumns+` FROM goal_admissions WHERE goal_id = ?`, request.GoalID)); lookupErr == nil {
		if existing.SessionID != request.SessionID || existing.TeamID != request.TeamID || existing.AgentID != request.AgentID || existing.InputDigest != request.InputDigest || existing.ContractDigest != request.ContractDigest || existing.ConfigDigest != request.ConfigDigest {
			return agentgoal.Admission{}, fmt.Errorf("%w: Goal %s already has a different admission", orchestration.ErrIdempotencyConflict, request.GoalID)
		}
		if err := tx.Commit(); err != nil {
			return agentgoal.Admission{}, err
		}
		return existing, nil
	} else if !errors.Is(lookupErr, sql.ErrNoRows) {
		return agentgoal.Admission{}, fmt.Errorf("inspect Goal admission by Goal ID: %w", lookupErr)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = tx.ExecContext(ctx, `INSERT INTO goal_admissions(`+admissionColumns+`) VALUES(?, ?, ?, ?, ?, ?, ?, ?, '', 0, ?, 1, '', ?, ?)`, request.Token, request.GoalID, request.SessionID, string(request.TeamID), string(request.AgentID), request.InputDigest, request.ContractDigest, request.ConfigDigest, agentgoal.AdmissionReserved, now, now)
	if err != nil {
		return agentgoal.Admission{}, fmt.Errorf("insert Goal admission: %w", err)
	}
	value, err := admissionByToken(ctx, tx, request.Token)
	if err != nil {
		return agentgoal.Admission{}, err
	}
	if err := tx.Commit(); err != nil {
		return agentgoal.Admission{}, fmt.Errorf("commit Goal admission: %w", err)
	}
	return value, nil
}

func (store *OrchestrationStore) BindTurn(ctx context.Context, token, turnID, inputDigest string, expected uint64) (agentgoal.Admission, error) {
	return store.advanceAdmission(ctx, token, inputDigest, expected, func(value *agentgoal.Admission) error {
		if strings.TrimSpace(turnID) == "" {
			return fmt.Errorf("%w: turn ID is required", orchestration.ErrInvalidEvent)
		}
		if value.State == agentgoal.AdmissionTurnStarted && value.TurnID == turnID {
			return nil
		}
		if value.State != agentgoal.AdmissionReserved {
			return fmt.Errorf("%w: admission is %s", agentgoal.ErrAdmissionTransition, value.State)
		}
		value.TurnID = turnID
		return nil
	}, agentgoal.AdmissionTurnStarted)
}

func (store *OrchestrationStore) BindRound(ctx context.Context, token, goalID string, round int, expected uint64) (agentgoal.Admission, error) {
	if strings.TrimSpace(goalID) == "" || round <= 0 {
		return agentgoal.Admission{}, fmt.Errorf("%w: Goal ID and positive round are required", orchestration.ErrInvalidEvent)
	}
	return store.advanceAdmission(ctx, token, "", expected, func(value *agentgoal.Admission) error {
		if value.State == agentgoal.AdmissionRoundStarted && value.GoalID == goalID && value.Round == round {
			return nil
		}
		if value.State != agentgoal.AdmissionTurnStarted || value.GoalID != goalID {
			return fmt.Errorf("%w: admission is not bound to this Goal round", agentgoal.ErrAdmissionTransition)
		}
		value.Round = round
		return nil
	}, agentgoal.AdmissionRoundStarted)
}

func (store *OrchestrationStore) Consume(ctx context.Context, token string, expected uint64) (agentgoal.Admission, error) {
	return store.advanceAdmission(ctx, token, "", expected, func(value *agentgoal.Admission) error {
		if value.State == agentgoal.AdmissionConsumed {
			return nil
		}
		if value.State != agentgoal.AdmissionRoundStarted {
			return fmt.Errorf("%w: admission is %s", agentgoal.ErrAdmissionTransition, value.State)
		}
		return nil
	}, agentgoal.AdmissionConsumed)
}

func (store *OrchestrationStore) MarkUnavailable(ctx context.Context, token string, expected uint64, reason string) (agentgoal.Admission, error) {
	if strings.TrimSpace(reason) == "" {
		return agentgoal.Admission{}, fmt.Errorf("%w: unavailable reason is required", orchestration.ErrInvalidEvent)
	}
	return store.advanceAdmission(ctx, token, "", expected, func(value *agentgoal.Admission) error {
		if value.State == agentgoal.AdmissionUnavailable {
			if value.Reason != reason {
				return fmt.Errorf("%w: unavailable reason differs", orchestration.ErrIdempotencyConflict)
			}
			return nil
		}
		if value.State == agentgoal.AdmissionConsumed {
			return fmt.Errorf("%w: consumed admission cannot be unavailable", agentgoal.ErrAdmissionTransition)
		}
		value.Reason = reason
		return nil
	}, agentgoal.AdmissionUnavailable)
}

func (store *OrchestrationStore) advanceAdmission(ctx context.Context, token, inputDigest string, expected uint64, prepare func(*agentgoal.Admission) error, target agentgoal.AdmissionState) (agentgoal.Admission, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(token) == "" {
		return agentgoal.Admission{}, fmt.Errorf("%w: admission token is required", orchestration.ErrInvalidEvent)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return agentgoal.Admission{}, err
	}
	defer func() { _ = tx.Rollback() }()
	value, err := admissionByToken(ctx, tx, token)
	if errors.Is(err, sql.ErrNoRows) {
		return agentgoal.Admission{}, fmt.Errorf("%w: Goal admission %s", orchestration.ErrNotFound, token)
	}
	if err != nil {
		return agentgoal.Admission{}, err
	}
	if inputDigest != "" && value.InputDigest != inputDigest {
		return agentgoal.Admission{}, fmt.Errorf("%w: admission input digest differs", agentgoal.ErrAdmissionTransition)
	}
	beforeState, beforeVersion, beforeTurn, beforeRound := value.State, value.Version, value.TurnID, value.Round
	if err := prepare(&value); err != nil {
		return agentgoal.Admission{}, err
	}
	if value.State == target && value.TurnID == beforeTurn && value.Round == beforeRound && beforeState == target { // idempotent terminal retry
		if err := tx.Commit(); err != nil {
			return agentgoal.Admission{}, err
		}
		return value, nil
	}
	if expected == 0 || expected != beforeVersion {
		return agentgoal.Admission{}, fmt.Errorf("%w: admission expected version %d, current %d", orchestration.ErrSequenceConflict, expected, beforeVersion)
	}
	if !agentgoal.ValidAdmissionTransition(beforeState, target) {
		return agentgoal.Admission{}, fmt.Errorf("%w: %s -> %s", agentgoal.ErrAdmissionTransition, beforeState, target)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, `UPDATE goal_admissions SET turn_id = ?, round = ?, state = ?, version = version + 1, reason = ?, updated_at = ? WHERE token = ? AND version = ? AND state = ?`, value.TurnID, value.Round, target, value.Reason, now, token, beforeVersion, beforeState)
	if err != nil {
		return agentgoal.Admission{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return agentgoal.Admission{}, err
	}
	if changed != 1 {
		return agentgoal.Admission{}, fmt.Errorf("%w: Goal admission CAS", orchestration.ErrSequenceConflict)
	}
	updated, err := admissionByToken(ctx, tx, token)
	if err != nil {
		return agentgoal.Admission{}, err
	}
	if err := tx.Commit(); err != nil {
		return agentgoal.Admission{}, err
	}
	return updated, nil
}

func (store *OrchestrationStore) Get(ctx context.Context, token string) (agentgoal.Admission, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	value, err := admissionByToken(ctx, store.db, token)
	if errors.Is(err, sql.ErrNoRows) {
		return agentgoal.Admission{}, fmt.Errorf("%w: Goal admission %s", orchestration.ErrNotFound, token)
	}
	return value, err
}

func (store *OrchestrationStore) List(ctx context.Context, state agentgoal.AdmissionState) ([]agentgoal.Admission, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := state.Validate(); err != nil {
		return nil, err
	}
	rows, err := store.db.QueryContext(ctx, `SELECT `+admissionColumns+` FROM goal_admissions WHERE state = ? ORDER BY created_at, token`, state)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]agentgoal.Admission, 0)
	for rows.Next() {
		value, scanErr := scanAdmission(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

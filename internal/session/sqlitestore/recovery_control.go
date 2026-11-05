package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

// AbandonRecovery is an offline-only transaction. The caller must hold the
// runtimeowner lock without constructing a runtime/Supervisor. This narrow
// operation owns consumer cancellation, audit and reference release together;
// there is deliberately no generic reference-delete or force-release API.
func (database *Database) AbandonRecovery(ctx context.Context, request session.AbandonRequest) (session.Abandonment, error) {
	var empty session.Abandonment
	if err := request.Validate(); err != nil {
		return empty, err
	}
	tx, err := database.db.BeginTx(ctx, nil)
	if err != nil {
		return empty, err
	}
	defer func() { _ = tx.Rollback() }()
	teamID, agentID, err := offlineRoot(ctx, tx, request.SessionID)
	if err != nil {
		return empty, err
	}
	goalID := request.GoalID
	if request.TurnID != "" {
		err := tx.QueryRowContext(ctx, `SELECT g.goal_id FROM goal_rounds r JOIN goal_runs g USING(goal_id) WHERE r.turn_id = ? AND g.session_id = ?`, request.TurnID, string(request.SessionID)).Scan(&goalID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return empty, err
		}
	}
	kind, targetID := "turn", request.TurnID
	var goal agentgoal.Goal
	if goalID != "" {
		kind, targetID = "goal", goalID
		goal, err = goalByID(ctx, tx, goalID)
		if err != nil {
			return empty, err
		}
		if goal.SessionID != request.SessionID || (goal.TeamID != "" && string(goal.TeamID) != teamID) || (goal.AgentID != "" && string(goal.AgentID) != agentID) {
			return empty, errors.New("goal does not belong to the selected root session")
		}
	}
	var stored []byte
	var storedSHA string
	err = tx.QueryRowContext(ctx, `SELECT receipt, receipt_sha FROM recovery_abandonments WHERE session_id = ? AND target_kind = ? AND target_id = ?`, string(request.SessionID), kind, targetID).Scan(&stored, &storedSHA)
	if err == nil {
		var receipt session.Abandonment
		if sha256Hex(stored) != storedSHA {
			return empty, errors.New("abandonment receipt checksum mismatch")
		}
		if err := json.Unmarshal(stored, &receipt); err != nil {
			return empty, err
		}
		if receipt.SessionID != request.SessionID || receipt.GoalID != goalID || receipt.Reason != request.Reason {
			return empty, orchestration.ErrIdempotencyConflict
		}
		if request.TurnID != "" && (len(receipt.TurnIDs) == 0 || receipt.TurnIDs[len(receipt.TurnIDs)-1] != request.TurnID) {
			return empty, orchestration.ErrIdempotencyConflict
		}
		return receipt, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return empty, err
	}
	if err := guardOfflineDependencies(ctx, tx, request.SessionID, teamID); err != nil {
		return empty, err
	}
	turnIDs := []string{request.TurnID}
	if goalID != "" {
		if goal.State != agentgoal.StateRunning {
			return empty, errors.New("goal is already terminal; no abandonment was recorded")
		}
		turnIDs, err = offlineGoalTurns(ctx, tx, goal)
		if err != nil {
			return empty, err
		}
		if request.TurnID != "" && (len(turnIDs) == 0 || turnIDs[len(turnIDs)-1] != request.TurnID) {
			return empty, errors.New("abandon must name the current Goal turn, not an earlier round")
		}
	}
	now := time.Now().UTC()
	receipt := session.Abandonment{SessionID: request.SessionID, GoalID: goalID, TurnIDs: turnIDs, Reason: request.Reason, AbandonedAt: now}
	var pending []session.PendingEvent
	partial := goal.Result
	for _, turnID := range turnIDs {
		state, err := inspectOfflineTurn(ctx, tx, request.SessionID, turnID)
		if err != nil {
			return empty, err
		}
		if state.abandoned {
			return empty, session.ErrRecoveryAbandoned
		}
		if goalID == "" && state.completed {
			return empty, errors.New("turn is already completed; no abandonment was recorded")
		}
		var referenceID string
		var released sql.NullString
		err = tx.QueryRowContext(ctx, `SELECT snapshot_id, released_at FROM runtime_snapshot_refs WHERE kind = 'turn' AND scope = ? AND consumer_id = ?`, string(request.SessionID), turnID).Scan(&referenceID, &released)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return empty, err
		}
		if goalID == "" && !state.started && referenceID == "" {
			return empty, fmt.Errorf("turn %q does not exist in session %q", turnID, request.SessionID)
		}
		if released.Valid {
			return empty, errors.New("reference already released without a matching abandonment receipt")
		}
		if referenceID != "" && state.snapshotID != "" && referenceID != state.snapshotID {
			return empty, errors.New("turn and retention reference disagree on snapshot identity")
		}
		if referenceID != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE runtime_snapshot_refs SET released_at = ?, release_reason = ? WHERE kind = 'turn' AND scope = ? AND consumer_id = ? AND released_at IS NULL`, now.Format(time.RFC3339Nano), request.Reason, string(request.SessionID), turnID); err != nil {
				return empty, err
			}
			receipt.ReleasedReferences++
		}
		if state.candidate != "" {
			partial = state.candidate
		}
		pending = append(pending, session.PendingEvent{Type: session.EventTurnAbandoned, CorrelationID: turnID, RuntimeSnapshotID: state.snapshotID, Payload: session.MustJSONPayload(map[string]any{"turn_id": turnID, "goal_id": goalID, "reason": request.Reason, "effects_reverted": false})})
	}
	if goalID != "" {
		if goal.Round > 0 {
			last, err := roundByNumber(ctx, tx, goalID, goal.Round)
			if err != nil {
				return empty, err
			}
			if last.Candidate != "" {
				partial = last.Candidate
			}
		}
		// Preserve candidates/reports/checkpoints as evidence. Cancellation is
		// not semantic completion and does not assert effects were undone.
		if err := execCAS(ctx, tx, orchestration.ErrSequenceConflict, `UPDATE goal_runs SET state = 'canceled', decision = '', stop_reason = ?, reason = ?, result = ?, version = version + 1, updated_at = ?, completed_at = ? WHERE goal_id = ? AND state = 'running' AND version = ?`, string(agentgoal.StopAbandoned), request.Reason, partial, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), goalID, goal.Version); err != nil {
			return empty, err
		}
		pending = append(pending, session.PendingEvent{Type: session.EventGoalAbandoned, CorrelationID: goalID, Payload: session.MustJSONPayload(receipt)})
	}
	if _, err := appendSessionEvents(ctx, tx, request.SessionID, session.AnySequence, pending...); err != nil {
		return empty, err
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return empty, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO recovery_abandonments(session_id, target_kind, target_id, receipt, receipt_sha) VALUES(?, ?, ?, ?, ?)`, string(request.SessionID), kind, targetID, encoded, sha256Hex(encoded)); err != nil {
		return empty, err
	}
	if err := tx.Commit(); err != nil {
		return empty, err
	}
	return receipt, nil
}

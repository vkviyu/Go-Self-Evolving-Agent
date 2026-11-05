package sqlitestore

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

// These projections share event decoding, not policy: retention validates a
// complete lifecycle; abandonment inspects one interrupted turn and preserves
// its candidate. Do not silently make either policy inherit the other's rules.
type retentionTurn struct {
	started, completed, closed bool
	snapshot                   string
}

func retentionTurns(ctx context.Context, query readQuery, id session.ID) (map[string]*retentionTurn, error) {
	rows, err := query.QueryContext(ctx, `SELECT `+sessionEventColumns+` FROM session_events WHERE session_id=? ORDER BY sequence`, string(id))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	turns := make(map[string]*retentionTurn)
	for rows.Next() {
		event, err := scanSessionEvent(rows, id)
		if err != nil {
			return nil, err
		}
		if event.Type != session.EventTurnStarted && event.Type != session.EventTurnCompleted && event.Type != session.EventTurnAbandoned && event.Type != session.EventTurnArchived {
			continue
		}
		if event.CorrelationID == "" {
			return nil, errors.New("turn lifecycle event lacks identity")
		}
		state := turns[event.CorrelationID]
		if state == nil {
			state = &retentionTurn{}
			turns[event.CorrelationID] = state
		}
		switch event.Type {
		case session.EventTurnStarted:
			if state.started || state.completed || state.closed {
				return nil, errors.New("inconsistent turn start in retention audit")
			}
			state.started, state.snapshot = true, event.RuntimeSnapshotID
		case session.EventTurnCompleted:
			if !state.started || state.completed || state.closed || (state.snapshot != "" && event.RuntimeSnapshotID != state.snapshot) {
				return nil, errors.New("inconsistent turn completion in retention audit")
			}
			state.completed = true
		default:
			state.closed = true
		}
	}
	return turns, rows.Err()
}

type offlineTurn struct {
	started, completed, abandoned bool
	snapshotID, candidate         string
}

func inspectOfflineTurn(ctx context.Context, query readQuery, id session.ID, turnID string) (offlineTurn, error) {
	var state offlineTurn
	rows, err := query.QueryContext(ctx, `SELECT `+sessionEventColumns+` FROM session_events WHERE session_id = ? AND correlation_id = ? ORDER BY sequence`, string(id), turnID)
	if err != nil {
		return state, err
	}
	defer rows.Close()
	for rows.Next() {
		event, err := scanSessionEvent(rows, id)
		if err != nil {
			return state, err
		}
		switch event.Type {
		case session.EventTurnStarted:
			if state.started {
				return state, errors.New("duplicate turn start")
			}
			state.started, state.snapshotID = true, event.RuntimeSnapshotID
		case session.EventTurnCompleted:
			if state.completed || (event.RuntimeSnapshotID != "" && state.snapshotID != "" && event.RuntimeSnapshotID != state.snapshotID) {
				return state, errors.New("inconsistent turn completion evidence")
			}
			state.completed = true
			var payload struct {
				Content string `json:"content"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return state, err
			}
			state.candidate = payload.Content
		case session.EventTurnAbandoned:
			state.abandoned = true
		}
	}
	return state, rows.Err()
}

func offlineGoalTurns(ctx context.Context, query readQuery, goal agentgoal.Goal) ([]string, error) {
	rows, err := query.QueryContext(ctx, `SELECT round_number, turn_id FROM goal_rounds WHERE goal_id = ? ORDER BY round_number LIMIT 1001`, goal.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	turns := make([]string, 0)
	for rows.Next() {
		var number int
		var turnID string
		if err := rows.Scan(&number, &turnID); err != nil {
			return nil, err
		}
		if number != len(turns)+1 || turnID == "" || len(turns) >= 1000 {
			return nil, errors.New("goal rounds are inconsistent or exceed offline abandonment limit")
		}
		turns = append(turns, turnID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(turns) != goal.Round {
		return nil, errors.New("goal current round does not match retained rounds")
	}
	return turns, nil
}

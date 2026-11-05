package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

func hashWaitRequest(spec orchestration.WaitSpec) string {
	targets := append([]orchestration.AgentID(nil), spec.TargetAgentIDs...)
	request := struct {
		TeamID         orchestration.TeamID    `json:"team_id"`
		AgentID        orchestration.AgentID   `json:"agent_id"`
		RunID          orchestration.RunID     `json:"run_id"`
		ToolCallID     string                  `json:"tool_call_id"`
		Condition      string                  `json:"condition"`
		TargetAgentIDs []orchestration.AgentID `json:"target_agent_ids"`
		Any            bool                    `json:"any"`
		Deadline       *time.Time              `json:"deadline,omitempty"`
		ResumeToken    string                  `json:"resume_token"`
	}{
		TeamID: spec.TeamID, AgentID: spec.AgentID, RunID: spec.RunID, ToolCallID: spec.ToolCallID,
		Condition: spec.Condition, TargetAgentIDs: targets, Any: spec.Any, Deadline: spec.Deadline,
		ResumeToken: spec.ResumeToken,
	}
	encoded, _ := json.Marshal(request)
	return sha256Hex(encoded)
}

func (store *OrchestrationStore) CreateWaitSet(ctx context.Context, spec orchestration.WaitSpec) (orchestration.WaitSet, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := spec.Validate(); err != nil {
		return orchestration.WaitSet{}, err
	}
	targetsJSON, err := orchestration.WaitSetTargetsJSON(spec.TargetAgentIDs)
	if err != nil {
		return orchestration.WaitSet{}, fmt.Errorf("encode wait targets: %w", err)
	}
	hash := hashWaitRequest(spec)
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return orchestration.WaitSet{}, fmt.Errorf("begin wait set creation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if spec.IdempotencyKey != "" {
		var waitID, storedHash string
		lookupErr := tx.QueryRowContext(ctx, `SELECT wait_set_id, request_hash FROM wait_sets WHERE team_id = ? AND idempotency_key = ?`, string(spec.TeamID), spec.IdempotencyKey).Scan(&waitID, &storedHash)
		if lookupErr == nil {
			if storedHash != "" && storedHash != hash {
				return orchestration.WaitSet{}, fmt.Errorf("%w: wait key %q", orchestration.ErrIdempotencyConflict, spec.IdempotencyKey)
			}
			waitSet, getErr := waitSetByID(ctx, tx, orchestration.WaitSetID(waitID))
			if getErr != nil {
				return orchestration.WaitSet{}, getErr
			}
			if storedHash == "" && !legacyWaitMatches(waitSet, spec) {
				return orchestration.WaitSet{}, fmt.Errorf("%w: legacy wait key %q", orchestration.ErrIdempotencyConflict, spec.IdempotencyKey)
			}
			if err := tx.Commit(); err != nil {
				return orchestration.WaitSet{}, fmt.Errorf("commit idempotent wait set lookup: %w", err)
			}
			return waitSet, nil
		}
		if !errors.Is(lookupErr, sql.ErrNoRows) {
			return orchestration.WaitSet{}, fmt.Errorf("inspect wait idempotency key: %w", lookupErr)
		}
	}
	agent, err := agentByID(ctx, tx, spec.AgentID)
	if errors.Is(err, sql.ErrNoRows) {
		return orchestration.WaitSet{}, fmt.Errorf("%w: Agent %s", orchestration.ErrNotFound, spec.AgentID)
	}
	if err != nil {
		return orchestration.WaitSet{}, err
	}
	if agent.TeamID != spec.TeamID {
		return orchestration.WaitSet{}, fmt.Errorf("%w: wait Agent belongs to another Team", orchestration.ErrInvalidEvent)
	}
	if agent.State != orchestration.AgentRunning {
		return orchestration.WaitSet{}, fmt.Errorf("%w: Agent %s must be running to suspend", orchestration.ErrInvalidEvent, agent.ID)
	}
	if spec.RunID != "" && agent.CurrentRunID != spec.RunID {
		return orchestration.WaitSet{}, fmt.Errorf("%w: wait run does not match current Agent run", orchestration.ErrAgentStateConflict)
	}
	for _, targetID := range spec.TargetAgentIDs {
		target, targetErr := agentByID(ctx, tx, targetID)
		if errors.Is(targetErr, sql.ErrNoRows) {
			return orchestration.WaitSet{}, fmt.Errorf("%w: wait target Agent %s", orchestration.ErrNotFound, targetID)
		}
		if targetErr != nil {
			return orchestration.WaitSet{}, targetErr
		}
		if target.TeamID != spec.TeamID {
			return orchestration.WaitSet{}, fmt.Errorf("%w: wait target Agent belongs to another Team", orchestration.ErrInvalidEvent)
		}
	}
	waitID, err := orchestration.NewWaitSetID()
	if err != nil {
		return orchestration.WaitSet{}, err
	}
	now := time.Now().UTC()
	deadline := ""
	if spec.Deadline != nil {
		deadline = spec.Deadline.UTC().Format(time.RFC3339Nano)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO wait_sets(wait_set_id, team_id, agent_id, run_id, tool_call_id, condition, target_agent_ids, wait_any, deadline, resume_token, state, correlation_id, idempotency_key, request_hash, created_at, updated_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?)`,
		string(waitID), string(spec.TeamID), string(spec.AgentID), string(spec.RunID), spec.ToolCallID, spec.Condition, targetsJSON, boolInt(spec.Any), deadline, spec.ResumeToken, orchestration.WaitSetPending, spec.CorrelationID, spec.IdempotencyKey, hash, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		return orchestration.WaitSet{}, fmt.Errorf("insert wait set: %w", err)
	}
	activity := "waiting:" + spec.Condition
	result, err := tx.ExecContext(ctx, `UPDATE agents SET state = ?, activity = ?, version = version + 1, updated_at = ? WHERE agent_id = ? AND state = ? AND version = ?`,
		string(orchestration.AgentWaiting), activity, now.Format(time.RFC3339Nano), string(agent.ID), string(orchestration.AgentRunning), agent.Version)
	if err != nil {
		return orchestration.WaitSet{}, fmt.Errorf("suspend Agent: %w", err)
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		if err != nil {
			return orchestration.WaitSet{}, fmt.Errorf("inspect suspended Agent update: %w", err)
		}
		return orchestration.WaitSet{}, fmt.Errorf("%w: Agent state changed while suspending", orchestration.ErrAgentStateConflict)
	}
	sequence, err := currentOrchestrationSequence(ctx, tx, orchestration.StreamAgent, string(agent.ID))
	if err != nil {
		return orchestration.WaitSet{}, err
	}
	pending := []orchestration.PendingEvent{
		{Type: orchestration.EventWaitSetCreated, ActorAgentID: agent.ID, CorrelationID: spec.CorrelationID, IdempotencyKey: spec.IdempotencyKey, Payload: orchestration.MustJSONPayload(map[string]any{"wait_set_id": waitID, "condition": spec.Condition, "target_agent_ids": spec.TargetAgentIDs, "any": spec.Any, "deadline": spec.Deadline, "resume_token": spec.ResumeToken})},
		{Type: orchestration.EventAgentWaiting, ActorAgentID: agent.ID, CorrelationID: spec.CorrelationID, Payload: orchestration.MustJSONPayload(map[string]any{"wait_set_id": waitID, "activity": activity, "run_id": spec.RunID})},
	}
	if _, err := appendOrchestrationEvents(ctx, tx, orchestration.Stream{Type: orchestration.StreamAgent, ID: string(agent.ID), TeamID: agent.TeamID}, sequence, pending...); err != nil {
		return orchestration.WaitSet{}, fmt.Errorf("record wait set creation: %w", err)
	}
	waitSet, err := waitSetByID(ctx, tx, waitID)
	if err != nil {
		return orchestration.WaitSet{}, err
	}
	if err := tx.Commit(); err != nil {
		return orchestration.WaitSet{}, fmt.Errorf("commit wait set creation: %w", err)
	}
	return waitSet, nil
}

func (store *OrchestrationStore) GetWaitSet(ctx context.Context, waitID orchestration.WaitSetID) (orchestration.WaitSet, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := waitID.Validate(); err != nil {
		return orchestration.WaitSet{}, fmt.Errorf("invalid wait set ID: %w", err)
	}
	waitSet, err := waitSetByID(ctx, store.db, waitID)
	if errors.Is(err, sql.ErrNoRows) {
		return orchestration.WaitSet{}, fmt.Errorf("%w: wait set %s", orchestration.ErrNotFound, waitID)
	}
	return waitSet, err
}

func (store *OrchestrationStore) ListWaitSets(ctx context.Context, query orchestration.WaitSetQuery) ([]orchestration.WaitSet, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := query.TeamID.Validate(); err != nil {
		return nil, fmt.Errorf("invalid wait query team: %w", err)
	}
	conditions := []string{"team_id = ?"}
	args := []any{string(query.TeamID)}
	if query.AgentID != "" {
		if err := query.AgentID.Validate(); err != nil {
			return nil, fmt.Errorf("invalid wait query Agent: %w", err)
		}
		conditions = append(conditions, "agent_id = ?")
		args = append(args, string(query.AgentID))
	}
	if len(query.States) > 0 {
		placeholders := make([]string, len(query.States))
		for index, state := range query.States {
			if state == "" {
				return nil, fmt.Errorf("%w: wait query state is empty", orchestration.ErrInvalidEvent)
			}
			placeholders[index] = "?"
			args = append(args, string(state))
		}
		conditions = append(conditions, "state IN ("+strings.Join(placeholders, ", ")+")")
	}
	rows, err := store.db.QueryContext(ctx, `SELECT wait_set_id, team_id, agent_id, run_id, tool_call_id, condition, target_agent_ids, wait_any, deadline, resume_token, state, correlation_id, idempotency_key, created_at, updated_at, resolved_at FROM wait_sets WHERE `+strings.Join(conditions, " AND ")+` ORDER BY created_at ASC, wait_set_id ASC`, args...)
	if err != nil {
		return nil, fmt.Errorf("list wait sets: %w", err)
	}
	defer rows.Close()
	result := make([]orchestration.WaitSet, 0)
	for rows.Next() {
		waitSet, scanErr := scanWaitSet(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, waitSet)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate wait sets: %w", err)
	}
	return result, nil
}

func (store *OrchestrationStore) ResolveWaitSet(ctx context.Context, waitID orchestration.WaitSetID, reason string) (orchestration.WaitSet, error) {
	return store.transitionWaitSet(ctx, waitID, orchestration.WaitSetResolved, orchestration.EventWaitSetResolved, reason)
}

// ConsumeWaitSet marks a resolved suspension as handed to a resumed runner.
// Keeping this separate from resolution makes retries crash-safe: a process
// may resume the same checkpoint again until dispatch has been recorded.
func (store *OrchestrationStore) ConsumeWaitSet(ctx context.Context, waitID orchestration.WaitSetID, reason string) (orchestration.WaitSet, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := waitID.Validate(); err != nil {
		return orchestration.WaitSet{}, fmt.Errorf("invalid wait set ID: %w", err)
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return orchestration.WaitSet{}, fmt.Errorf("begin wait set consumption: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	waitSet, err := waitSetByID(ctx, tx, waitID)
	if errors.Is(err, sql.ErrNoRows) {
		return orchestration.WaitSet{}, fmt.Errorf("%w: wait set %s", orchestration.ErrNotFound, waitID)
	}
	if err != nil {
		return orchestration.WaitSet{}, err
	}
	if waitSet.State != orchestration.WaitSetResolved && waitSet.State != orchestration.WaitSetExpired {
		if err := tx.Commit(); err != nil {
			return orchestration.WaitSet{}, fmt.Errorf("commit unchanged consumed wait set: %w", err)
		}
		return waitSet, nil
	}
	now := time.Now().UTC()
	if err := execCAS(ctx, tx, orchestration.ErrAgentStateConflict, `UPDATE wait_sets SET state = ?, updated_at = ? WHERE wait_set_id = ? AND state IN (?, ?)`,
		string(orchestration.WaitSetConsumed), now.Format(time.RFC3339Nano), string(waitID), string(orchestration.WaitSetResolved), string(orchestration.WaitSetExpired)); err != nil {
		return orchestration.WaitSet{}, fmt.Errorf("consume wait set: %w", err)
	}
	sequence, err := currentOrchestrationSequence(ctx, tx, orchestration.StreamAgent, string(waitSet.AgentID))
	if err != nil {
		return orchestration.WaitSet{}, err
	}
	if _, err := appendOrchestrationEvents(ctx, tx, orchestration.Stream{Type: orchestration.StreamAgent, ID: string(waitSet.AgentID), TeamID: waitSet.TeamID}, sequence,
		orchestration.PendingEvent{Type: orchestration.EventWaitSetConsumed, ActorAgentID: waitSet.AgentID, CorrelationID: waitSet.CorrelationID, Payload: orchestration.MustJSONPayload(map[string]any{"wait_set_id": waitID, "reason": reason})},
	); err != nil {
		return orchestration.WaitSet{}, fmt.Errorf("record wait set consumption: %w", err)
	}
	updated, err := waitSetByID(ctx, tx, waitID)
	if err != nil {
		return orchestration.WaitSet{}, err
	}
	if err := tx.Commit(); err != nil {
		return orchestration.WaitSet{}, fmt.Errorf("commit wait set consumption: %w", err)
	}
	return updated, nil
}

func (store *OrchestrationStore) ExpireWaitSet(ctx context.Context, waitID orchestration.WaitSetID, reason string) (orchestration.WaitSet, error) {
	return store.transitionWaitSet(ctx, waitID, orchestration.WaitSetExpired, orchestration.EventWaitSetExpired, reason)
}

func (store *OrchestrationStore) CancelWaitSetsForAgent(ctx context.Context, agentID orchestration.AgentID, reason string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := agentID.Validate(); err != nil {
		return err
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin wait set cancellation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	agent, err := agentByID(ctx, tx, agentID)
	if err != nil {
		return recordError(err, "Agent", string(agentID))
	}
	rows, err := tx.QueryContext(ctx, `SELECT wait_set_id FROM wait_sets WHERE team_id = ? AND agent_id = ? AND state = ? ORDER BY created_at, wait_set_id`, string(agent.TeamID), string(agentID), string(orchestration.WaitSetPending))
	if err != nil {
		return err
	}
	var waitIDs []orchestration.WaitSetID
	for rows.Next() {
		var id orchestration.WaitSetID
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		waitIDs = append(waitIDs, id)
	}
	if err := closeRetentionRows(rows); err != nil {
		return err
	}
	for _, waitID := range waitIDs {
		if _, err := transitionWaitSetTx(ctx, tx, waitID, orchestration.WaitSetCanceled, orchestration.EventWaitSetCanceled, reason); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (store *OrchestrationStore) transitionWaitSet(ctx context.Context, waitID orchestration.WaitSetID, target orchestration.WaitSetState, eventType orchestration.EventType, reason string) (orchestration.WaitSet, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := waitID.Validate(); err != nil {
		return orchestration.WaitSet{}, fmt.Errorf("invalid wait set ID: %w", err)
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return orchestration.WaitSet{}, fmt.Errorf("begin wait set transition: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	updated, err := transitionWaitSetTx(ctx, tx, waitID, target, eventType, reason)
	if err != nil {
		return orchestration.WaitSet{}, err
	}
	if err := tx.Commit(); err != nil {
		return orchestration.WaitSet{}, fmt.Errorf("commit wait set transition: %w", err)
	}
	return updated, nil
}

// transitionWaitSetTx lets batch cancellation commit the entire set of state
// changes and events together, rather than leaving a partially canceled set.
func transitionWaitSetTx(ctx context.Context, tx *sql.Tx, waitID orchestration.WaitSetID, target orchestration.WaitSetState, eventType orchestration.EventType, reason string) (orchestration.WaitSet, error) {
	waitSet, err := waitSetByID(ctx, tx, waitID)
	if errors.Is(err, sql.ErrNoRows) {
		return orchestration.WaitSet{}, fmt.Errorf("%w: wait set %s", orchestration.ErrNotFound, waitID)
	}
	if err != nil {
		return orchestration.WaitSet{}, err
	}
	if waitSet.State != orchestration.WaitSetPending {
		return waitSet, nil
	}
	if target != orchestration.WaitSetResolved && target != orchestration.WaitSetExpired && target != orchestration.WaitSetCanceled {
		return orchestration.WaitSet{}, fmt.Errorf("%w: unsupported wait set target %q", orchestration.ErrInvalidEvent, target)
	}
	agent, err := agentByID(ctx, tx, waitSet.AgentID)
	if err != nil {
		return orchestration.WaitSet{}, err
	}
	now := time.Now().UTC()
	if err := execCAS(ctx, tx, orchestration.ErrAgentStateConflict, `UPDATE wait_sets SET state = ?, updated_at = ?, resolved_at = ? WHERE wait_set_id = ? AND state = ?`,
		string(target), now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), string(waitID), string(orchestration.WaitSetPending)); err != nil {
		return orchestration.WaitSet{}, fmt.Errorf("update wait set: %w", err)
	}
	if agent.State == orchestration.AgentWaiting {
		result, err := tx.ExecContext(ctx, `UPDATE agents SET state = ?, activity = ?, version = version + 1, updated_at = ? WHERE agent_id = ? AND state = ? AND version = ?`,
			string(orchestration.AgentQueued), "wait_resolved", now.Format(time.RFC3339Nano), string(agent.ID), string(orchestration.AgentWaiting), agent.Version)
		if err != nil {
			return orchestration.WaitSet{}, fmt.Errorf("queue resumed Agent: %w", err)
		}
		if changed, err := result.RowsAffected(); err != nil || changed != 1 {
			if err != nil {
				return orchestration.WaitSet{}, fmt.Errorf("inspect resumed Agent update: %w", err)
			}
			return orchestration.WaitSet{}, fmt.Errorf("%w: Agent state changed while resolving wait", orchestration.ErrAgentStateConflict)
		}
	}
	sequence, err := currentOrchestrationSequence(ctx, tx, orchestration.StreamAgent, string(agent.ID))
	if err != nil {
		return orchestration.WaitSet{}, err
	}
	pending := []orchestration.PendingEvent{
		{Type: eventType, ActorAgentID: agent.ID, CorrelationID: waitSet.CorrelationID, Payload: orchestration.MustJSONPayload(map[string]any{"wait_set_id": waitID, "from": orchestration.WaitSetPending, "to": target, "reason": reason})},
		{Type: orchestration.EventAgentWaitResolved, ActorAgentID: agent.ID, CorrelationID: waitSet.CorrelationID, Payload: orchestration.MustJSONPayload(map[string]any{"wait_set_id": waitID, "state": target, "reason": reason})},
	}
	if agent.State == orchestration.AgentWaiting {
		pending = append(pending, orchestration.PendingEvent{Type: orchestration.EventAgentQueued, ActorAgentID: agent.ID, CorrelationID: waitSet.CorrelationID, Payload: orchestration.MustJSONPayload(map[string]any{"reason": "wait condition resolved", "wait_set_id": waitID})})
	}
	if _, err := appendOrchestrationEvents(ctx, tx, orchestration.Stream{Type: orchestration.StreamAgent, ID: string(agent.ID), TeamID: agent.TeamID}, sequence, pending...); err != nil {
		return orchestration.WaitSet{}, fmt.Errorf("record wait set transition: %w", err)
	}
	return waitSetByID(ctx, tx, waitID)
}

func waitSetByID(ctx context.Context, query rowQuery, waitID orchestration.WaitSetID) (orchestration.WaitSet, error) {
	return scanWaitSet(query.QueryRowContext(ctx, `SELECT wait_set_id, team_id, agent_id, run_id, tool_call_id, condition, target_agent_ids, wait_any, deadline, resume_token, state, correlation_id, idempotency_key, created_at, updated_at, resolved_at FROM wait_sets WHERE wait_set_id = ?`, string(waitID)))
}

func scanWaitSet(scanner rowScanner) (orchestration.WaitSet, error) {
	var waitSet orchestration.WaitSet
	var waitID, teamID, agentID, runID, condition, state, correlationID, idempotencyKey string
	var toolCallID, resumeToken string
	var targets []byte
	var anyValue int
	var deadline, resolvedAt sql.NullString
	var createdAt, updatedAt string
	if err := scanner.Scan(&waitID, &teamID, &agentID, &runID, &toolCallID, &condition, &targets, &anyValue, &deadline, &resumeToken, &state, &correlationID, &idempotencyKey, &createdAt, &updatedAt, &resolvedAt); err != nil {
		return orchestration.WaitSet{}, err
	}
	if err := json.Unmarshal(targets, &waitSet.TargetAgentIDs); err != nil {
		return orchestration.WaitSet{}, fmt.Errorf("decode wait targets: %w", err)
	}
	waitSet.ID = orchestration.WaitSetID(waitID)
	waitSet.TeamID = orchestration.TeamID(teamID)
	waitSet.AgentID = orchestration.AgentID(agentID)
	waitSet.RunID = orchestration.RunID(runID)
	waitSet.ToolCallID = toolCallID
	waitSet.Condition = condition
	waitSet.Any = anyValue != 0
	waitSet.ResumeToken = resumeToken
	waitSet.State = orchestration.WaitSetState(state)
	waitSet.CorrelationID = correlationID
	waitSet.IdempotencyKey = idempotencyKey
	var err error
	waitSet.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return orchestration.WaitSet{}, fmt.Errorf("parse wait creation time: %w", err)
	}
	waitSet.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return orchestration.WaitSet{}, fmt.Errorf("parse wait update time: %w", err)
	}
	if deadline.Valid && deadline.String != "" {
		value, parseErr := time.Parse(time.RFC3339Nano, deadline.String)
		if parseErr != nil {
			return orchestration.WaitSet{}, fmt.Errorf("parse wait deadline: %w", parseErr)
		}
		waitSet.Deadline = &value
	}
	if resolvedAt.Valid && resolvedAt.String != "" {
		value, parseErr := time.Parse(time.RFC3339Nano, resolvedAt.String)
		if parseErr != nil {
			return orchestration.WaitSet{}, fmt.Errorf("parse wait resolved time: %w", parseErr)
		}
		waitSet.ResolvedAt = &value
	}
	return waitSet, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

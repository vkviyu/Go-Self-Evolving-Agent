package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

func (store *OrchestrationStore) TransitionAgent(ctx context.Context, transition orchestration.AgentTransition) (orchestration.AgentSnapshot, error) {
	if err := transition.AgentID.Validate(); err != nil {
		return orchestration.AgentSnapshot{}, fmt.Errorf("invalid Agent transition ID: %w", err)
	}
	if transition.Target == "" {
		return orchestration.AgentSnapshot{}, fmt.Errorf("%w: Agent transition target is required", orchestration.ErrInvalidEvent)
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return orchestration.AgentSnapshot{}, fmt.Errorf("begin Agent transition: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	agent, err := agentByID(ctx, tx, transition.AgentID)
	if errors.Is(err, sql.ErrNoRows) {
		return orchestration.AgentSnapshot{}, fmt.Errorf("%w: Agent %s", orchestration.ErrNotFound, transition.AgentID)
	}
	if err != nil {
		return orchestration.AgentSnapshot{}, err
	}
	if transition.Expected != orchestration.AnySequence && transition.Expected != agent.Version {
		return orchestration.AgentSnapshot{}, fmt.Errorf("%w: expected %d, current %d", orchestration.ErrAgentStateConflict, transition.Expected, agent.Version)
	}
	if err := orchestration.ValidateAgentTransition(agent.State, transition.Target); err != nil {
		return orchestration.AgentSnapshot{}, err
	}
	if isTerminalAgentState(agent.State) && transition.Target == orchestration.AgentQueued {
		var pending bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM agent_completions WHERE agent_id = ? AND run_id = ? AND settled = 0)`, string(agent.ID), string(agent.CurrentRunID)).Scan(&pending); err != nil {
			return orchestration.AgentSnapshot{}, err
		}
		if pending {
			return orchestration.AgentSnapshot{}, fmt.Errorf("%w: previous Run result is still settling", orchestration.ErrAgentStateConflict)
		}
	}
	if transition.Result != nil {
		if err := transition.Result.Validate(); err != nil {
			return orchestration.AgentSnapshot{}, fmt.Errorf("validate Agent result: %w", err)
		}
	}
	resultPayload := []byte{}
	if transition.Result != nil {
		resultPayload, err = json.Marshal(transition.Result)
		if err != nil {
			return orchestration.AgentSnapshot{}, fmt.Errorf("encode Agent result: %w", err)
		}
	}
	now := time.Now().UTC()
	if err := execCAS(ctx, tx, orchestration.ErrAgentStateConflict, `UPDATE agents SET state = ?, activity = ?, current_run_id = CASE WHEN ? <> '' THEN ? ELSE current_run_id END, result = ?, version = version + 1, updated_at = ? WHERE agent_id = ? AND version = ?`,
		string(transition.Target), transition.Activity, string(transition.RunID), string(transition.RunID), resultPayload, now.Format(time.RFC3339Nano), string(agent.ID), agent.Version); err != nil {
		return orchestration.AgentSnapshot{}, fmt.Errorf("update Agent state: %w", err)
	}
	eventType, err := agentTransitionEvent(transition.Target)
	if err != nil {
		return orchestration.AgentSnapshot{}, err
	}
	streamSequence, err := currentOrchestrationSequence(ctx, tx, orchestration.StreamAgent, string(agent.ID))
	if err != nil {
		return orchestration.AgentSnapshot{}, err
	}
	pendingEvents := []orchestration.PendingEvent{{Type: eventType, CausationID: transition.CausationID, CorrelationID: transition.CorrelationID, Payload: orchestration.MustJSONPayload(map[string]any{
		"from": agent.State, "to": transition.Target, "activity": transition.Activity, "run_id": transition.RunID, "reason": transition.Reason, "result": transition.Result,
	})}}
	if transition.Target == orchestration.AgentRunning {
		pendingEvents = append([]orchestration.PendingEvent{{Type: orchestration.EventAgentRunAdmitted, CausationID: transition.CausationID, CorrelationID: transition.CorrelationID, Payload: orchestration.MustJSONPayload(map[string]any{
			"run_id": transition.RunID, "agent_id": agent.ID,
		})}}, pendingEvents...)
	}
	if transition.Result != nil && (transition.Target == orchestration.AgentCompleted || transition.Target == orchestration.AgentFailed || transition.Target == orchestration.AgentCanceled) {
		runEventType := orchestration.EventAgentRunCompleted
		if transition.Target == orchestration.AgentFailed {
			runEventType = orchestration.EventAgentRunFailed
		}
		resultEvents := []orchestration.PendingEvent{{Type: orchestration.EventAgentResultRecorded, CausationID: transition.CausationID, CorrelationID: transition.CorrelationID, Payload: orchestration.MustJSONPayload(map[string]any{
			"result": transition.Result,
		})}}
		if transition.Target != orchestration.AgentCanceled {
			resultEvents = append(resultEvents, orchestration.PendingEvent{Type: runEventType, CausationID: transition.CausationID, CorrelationID: transition.CorrelationID, Payload: orchestration.MustJSONPayload(map[string]any{
				"result": transition.Result,
			})})
		}
		pendingEvents = append(resultEvents, pendingEvents...)
	}
	if _, err := appendOrchestrationEvents(ctx, tx, orchestration.Stream{Type: orchestration.StreamAgent, ID: string(agent.ID), TeamID: agent.TeamID}, streamSequence, pendingEvents...); err != nil {
		return orchestration.AgentSnapshot{}, fmt.Errorf("record Agent transition: %w", err)
	}
	updated, err := agentByID(ctx, tx, agent.ID)
	if err != nil {
		return orchestration.AgentSnapshot{}, err
	}
	// Terminal result and parent notification commit together. The mailbox key
	// makes the post-transition repair path idempotent.
	if isTerminalAgentState(updated.State) {
		spec, notify, err := orchestration.ResultMessage(updated)
		if err != nil {
			return orchestration.AgentSnapshot{}, fmt.Errorf("encode Agent result notification: %w", err)
		}
		if notify {
			if _, err := enqueueMessageTx(ctx, tx, spec); err != nil {
				return orchestration.AgentSnapshot{}, fmt.Errorf("record Agent result notification: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return orchestration.AgentSnapshot{}, fmt.Errorf("commit Agent transition: %w", err)
	}
	return updated, nil
}

func agentTransitionEvent(state orchestration.AgentState) (orchestration.EventType, error) {
	switch state {
	case orchestration.AgentQueued:
		return orchestration.EventAgentQueued, nil
	case orchestration.AgentRunning:
		return orchestration.EventAgentRunStarted, nil
	case orchestration.AgentWaiting:
		return orchestration.EventAgentWaiting, nil
	case orchestration.AgentCanceling:
		return orchestration.EventAgentCancelRequested, nil
	case orchestration.AgentCompleted:
		return orchestration.EventAgentCompleted, nil
	case orchestration.AgentFailed:
		return orchestration.EventAgentFailed, nil
	case orchestration.AgentCanceled:
		return orchestration.EventAgentCanceled, nil
	default:
		return "", fmt.Errorf("%w: unsupported Agent target %q", orchestration.ErrInvalidEvent, state)
	}
}

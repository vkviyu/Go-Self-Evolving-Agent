package atestore

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

type waitReceipt struct {
	ID   orchestration.WaitSetID
	Hash string
}

func hashWaitRequest(spec orchestration.WaitSpec) string {
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
	}{spec.TeamID, spec.AgentID, spec.RunID, spec.ToolCallID, spec.Condition, spec.TargetAgentIDs, spec.Any, spec.Deadline, spec.ResumeToken}
	data, _ := json.Marshal(request)
	return sha256Hex(data)
}
func (store *Store) CreateWaitSet(ctx context.Context, spec orchestration.WaitSpec) (result orchestration.WaitSet, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err = spec.Validate(); err != nil {
		return
	}
	id, err := orchestration.NewWaitSetID()
	if err != nil {
		return result, err
	}
	now := time.Now().UTC()
	err = store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		if spec.IdempotencyKey != "" {
			var receipt waitReceipt
			found, e := tx.Get(recordKey("wait-receipt", scopedKey(spec.TeamID, spec.IdempotencyKey)), &receipt)
			if e != nil {
				return e
			}
			if found {
				if receipt.Hash != hashWaitRequest(spec) {
					return fmt.Errorf("%w: wait key %q", orchestration.ErrIdempotencyConflict, spec.IdempotencyKey)
				}
				result, e = getRecord[orchestration.WaitSet](tx, "wait", string(receipt.ID))
				return e
			}
		}
		agent, e := agentTx(tx, spec.AgentID)
		if e != nil {
			return e
		}
		if agent.TeamID != spec.TeamID {
			return fmt.Errorf("%w: wait Agent belongs to another Team", orchestration.ErrInvalidEvent)
		}
		if agent.State != orchestration.AgentRunning {
			return fmt.Errorf("%w: Agent must be running to suspend", orchestration.ErrInvalidEvent)
		}
		if spec.RunID != "" && agent.CurrentRunID != spec.RunID {
			return fmt.Errorf("%w: wait run does not match current Agent run", orchestration.ErrAgentStateConflict)
		}
		for _, targetID := range spec.TargetAgentIDs {
			target, e := agentTx(tx, targetID)
			if e != nil {
				return e
			}
			if target.TeamID != spec.TeamID {
				return fmt.Errorf("%w: wait target belongs to another Team", orchestration.ErrInvalidEvent)
			}
		}
		wait := orchestration.WaitSet{ID: id, TeamID: spec.TeamID, AgentID: spec.AgentID, RunID: spec.RunID, ToolCallID: spec.ToolCallID, Condition: spec.Condition, TargetAgentIDs: append([]orchestration.AgentID(nil), spec.TargetAgentIDs...), Any: spec.Any, Deadline: spec.Deadline, ResumeToken: spec.ResumeToken, State: orchestration.WaitSetPending, CorrelationID: spec.CorrelationID, IdempotencyKey: spec.IdempotencyKey, CreatedAt: now, UpdatedAt: now}
		if e := createRecord(tx, "wait", string(id), wait); e != nil {
			return e
		}
		if spec.IdempotencyKey != "" {
			if e := createRecord(tx, "wait-receipt", scopedKey(spec.TeamID, spec.IdempotencyKey), waitReceipt{ID: id, Hash: hashWaitRequest(spec)}); e != nil {
				return e
			}
		}
		activity := "waiting:" + spec.Condition
		agent.State, agent.Activity, agent.UpdatedAt = orchestration.AgentWaiting, activity, now
		agent.Version++
		if e := putRecord(tx, "agent", string(agent.ID), agent); e != nil {
			return e
		}
		if _, e := appendEvents(tx, orchestration.Stream{Type: orchestration.StreamAgent, ID: string(agent.ID), TeamID: agent.TeamID}, orchestration.AnySequence,
			orchestration.PendingEvent{Type: orchestration.EventWaitSetCreated, ActorAgentID: agent.ID, CorrelationID: spec.CorrelationID, IdempotencyKey: spec.IdempotencyKey, Payload: orchestration.MustJSONPayload(map[string]any{"wait_set_id": id, "condition": spec.Condition, "target_agent_ids": spec.TargetAgentIDs, "any": spec.Any, "deadline": spec.Deadline, "resume_token": spec.ResumeToken})},
			orchestration.PendingEvent{Type: orchestration.EventAgentWaiting, ActorAgentID: agent.ID, CorrelationID: spec.CorrelationID, Payload: orchestration.MustJSONPayload(map[string]any{"wait_set_id": id, "activity": activity, "run_id": spec.RunID})}); e != nil {
			return e
		}
		result = wait
		return nil
	})
	return
}
func (store *Store) GetWaitSet(ctx context.Context, id orchestration.WaitSetID) (result orchestration.WaitSet, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err = id.Validate(); err != nil {
		return
	}
	err = store.authority.View(ctx, func(tx *ateauthority.Tx) error {
		var e error
		result, e = getRecord[orchestration.WaitSet](tx, "wait", string(id))
		return e
	})
	return
}
func waitSetsTx(tx *ateauthority.Tx, match func(orchestration.WaitSet) bool) ([]orchestration.WaitSet, error) {
	all, err := listRecords[orchestration.WaitSet](tx, "wait")
	if err != nil {
		return nil, err
	}
	result := make([]orchestration.WaitSet, 0)
	for _, wait := range all {
		if match(wait) {
			result = append(result, wait)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})
	return result, nil
}
func (store *Store) ListWaitSets(ctx context.Context, query orchestration.WaitSetQuery) (result []orchestration.WaitSet, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err = query.TeamID.Validate(); err != nil {
		return
	}
	if query.AgentID != "" {
		if err = query.AgentID.Validate(); err != nil {
			return
		}
	}
	if slices.Contains(query.States, "") {
		return nil, fmt.Errorf("%w: wait query state is empty", orchestration.ErrInvalidEvent)
	}
	err = store.authority.View(ctx, func(tx *ateauthority.Tx) error {
		var e error
		result, e = waitSetsTx(tx, func(w orchestration.WaitSet) bool {
			return w.TeamID == query.TeamID && (query.AgentID == "" || w.AgentID == query.AgentID) && (len(query.States) == 0 || slices.Contains(query.States, w.State))
		})
		return e
	})
	return
}
func (store *Store) ResolveWaitSet(ctx context.Context, id orchestration.WaitSetID, reason string) (orchestration.WaitSet, error) {
	return store.transitionWaitSet(ctx, id, orchestration.WaitSetResolved, orchestration.EventWaitSetResolved, reason)
}
func (store *Store) ExpireWaitSet(ctx context.Context, id orchestration.WaitSetID, reason string) (orchestration.WaitSet, error) {
	return store.transitionWaitSet(ctx, id, orchestration.WaitSetExpired, orchestration.EventWaitSetExpired, reason)
}
func (store *Store) ConsumeWaitSet(ctx context.Context, id orchestration.WaitSetID, reason string) (orchestration.WaitSet, error) {
	return store.transitionWaitSet(ctx, id, orchestration.WaitSetConsumed, orchestration.EventWaitSetConsumed, reason)
}
func (store *Store) transitionWaitSet(ctx context.Context, id orchestration.WaitSetID, target orchestration.WaitSetState, kind orchestration.EventType, reason string) (result orchestration.WaitSet, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err = id.Validate(); err != nil {
		return
	}
	now := time.Now().UTC()
	err = store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		var e error
		result, e = transitionWaitSetTx(tx, id, target, kind, reason, now)
		return e
	})
	return
}
func transitionWaitSetTx(tx *ateauthority.Tx, id orchestration.WaitSetID, target orchestration.WaitSetState, kind orchestration.EventType, reason string, now time.Time) (orchestration.WaitSet, error) {
	wait, err := getRecord[orchestration.WaitSet](tx, "wait", string(id))
	if err != nil {
		return wait, err
	}
	if target == orchestration.WaitSetConsumed {
		if wait.State != orchestration.WaitSetResolved && wait.State != orchestration.WaitSetExpired {
			return wait, nil
		}
		wait.State, wait.UpdatedAt = target, now
		if err := putRecord(tx, "wait", string(id), wait); err != nil {
			return wait, err
		}
		_, err := appendEvents(tx, orchestration.Stream{Type: orchestration.StreamAgent, ID: string(wait.AgentID), TeamID: wait.TeamID}, orchestration.AnySequence, orchestration.PendingEvent{Type: kind, ActorAgentID: wait.AgentID, CorrelationID: wait.CorrelationID, Payload: orchestration.MustJSONPayload(map[string]any{"wait_set_id": id, "reason": reason})})
		return wait, err
	}
	if wait.State != orchestration.WaitSetPending {
		return wait, nil
	}
	if target != orchestration.WaitSetResolved && target != orchestration.WaitSetExpired && target != orchestration.WaitSetCanceled {
		return wait, fmt.Errorf("%w: unsupported wait target %q", orchestration.ErrInvalidEvent, target)
	}
	agent, err := agentTx(tx, wait.AgentID)
	if err != nil {
		return wait, err
	}
	wait.State, wait.UpdatedAt, wait.ResolvedAt = target, now, &now
	if err := putRecord(tx, "wait", string(id), wait); err != nil {
		return wait, err
	}
	pending := []orchestration.PendingEvent{
		{Type: kind, ActorAgentID: agent.ID, CorrelationID: wait.CorrelationID, Payload: orchestration.MustJSONPayload(map[string]any{"wait_set_id": id, "from": orchestration.WaitSetPending, "to": target, "reason": reason})},
		{Type: orchestration.EventAgentWaitResolved, ActorAgentID: agent.ID, CorrelationID: wait.CorrelationID, Payload: orchestration.MustJSONPayload(map[string]any{"wait_set_id": id, "state": target, "reason": reason})},
	}
	if agent.State == orchestration.AgentWaiting {
		agent.State, agent.Activity, agent.UpdatedAt = orchestration.AgentQueued, "wait_resolved", now
		agent.Version++
		if err := putRecord(tx, "agent", string(agent.ID), agent); err != nil {
			return wait, err
		}
		pending = append(pending, orchestration.PendingEvent{Type: orchestration.EventAgentQueued, ActorAgentID: agent.ID, CorrelationID: wait.CorrelationID, Payload: orchestration.MustJSONPayload(map[string]any{"reason": "wait condition resolved", "wait_set_id": id})})
	}
	_, err = appendEvents(tx, orchestration.Stream{Type: orchestration.StreamAgent, ID: string(agent.ID), TeamID: agent.TeamID}, orchestration.AnySequence, pending...)
	return wait, err
}
func (store *Store) CancelWaitSetsForAgent(ctx context.Context, id orchestration.AgentID, reason string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := id.Validate(); err != nil {
		return err
	}
	now := time.Now().UTC()
	return store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		agent, err := agentTx(tx, id)
		if err != nil {
			return err
		}
		waits, err := waitSetsTx(tx, func(w orchestration.WaitSet) bool {
			return w.TeamID == agent.TeamID && w.AgentID == id && w.State == orchestration.WaitSetPending
		})
		if err != nil {
			return err
		}
		for _, wait := range waits {
			if _, err := transitionWaitSetTx(tx, wait.ID, orchestration.WaitSetCanceled, orchestration.EventWaitSetCanceled, reason, now); err != nil {
				return err
			}
		}
		return nil
	})
}

var _ orchestration.WaitRepository = (*Store)(nil)

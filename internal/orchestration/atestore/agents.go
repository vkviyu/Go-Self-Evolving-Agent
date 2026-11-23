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

func agentTx(tx *ateauthority.Tx, id orchestration.AgentID) (orchestration.AgentSnapshot, error) {
	agent, err := getRecord[orchestration.AgentSnapshot](tx, "agent", string(id))
	if err != nil {
		return agent, err
	}
	_, err = tx.Get(recordKey("agent-prompt", string(id)), &agent.InitialPrompt)
	return agent, err
}
func (store *Store) GetAgent(ctx context.Context, id orchestration.AgentID) (result orchestration.AgentSnapshot, err error) {
	if err = id.Validate(); err != nil {
		return
	}
	err = store.authority.View(ctx, func(tx *ateauthority.Tx) error { var e error; result, e = agentTx(tx, id); return e })
	return
}
func (store *Store) ListAgents(ctx context.Context, query orchestration.AgentQuery) (result []orchestration.AgentSnapshot, err error) {
	if err = query.TeamID.Validate(); err != nil {
		return
	}
	if query.ParentAgentID != "" {
		if err = query.ParentAgentID.Validate(); err != nil {
			return
		}
	}
	if slices.Contains(query.States, "") {
		return nil, fmt.Errorf("%w: Agent query state is empty", orchestration.ErrInvalidEvent)
	}
	err = store.authority.View(ctx, func(tx *ateauthority.Tx) error {
		agents, e := listRecords[orchestration.AgentSnapshot](tx, "agent")
		if e != nil {
			return e
		}
		result = make([]orchestration.AgentSnapshot, 0)
		for _, a := range agents {
			if a.TeamID != query.TeamID || (query.ParentAgentID != "" && a.ParentAgentID != query.ParentAgentID) || (len(query.States) > 0 && !slices.Contains(query.States, a.State)) {
				continue
			}
			if _, e := tx.Get(recordKey("agent-prompt", string(a.ID)), &a.InitialPrompt); e != nil {
				return e
			}
			result = append(result, a)
		}
		sort.Slice(result, func(i, j int) bool {
			if result[i].CreatedAt.Equal(result[j].CreatedAt) {
				return result[i].ID < result[j].ID
			}
			return result[i].CreatedAt.Before(result[j].CreatedAt)
		})
		return nil
	})
	return
}
func isTerminalAgentState(state orchestration.AgentState) bool {
	return state == orchestration.AgentCompleted || state == orchestration.AgentFailed || state == orchestration.AgentCanceled
}
func (store *Store) TransitionAgent(ctx context.Context, transition orchestration.AgentTransition) (result orchestration.AgentSnapshot, err error) {
	if err = transition.AgentID.Validate(); err != nil {
		return
	}
	if transition.Target == "" {
		return result, fmt.Errorf("%w: Agent transition target is required", orchestration.ErrInvalidEvent)
	}
	now := time.Now().UTC()
	err = store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		agent, e := agentTx(tx, transition.AgentID)
		if e != nil {
			return e
		}
		if transition.Expected != orchestration.AnySequence && transition.Expected != agent.Version {
			return fmt.Errorf("%w: expected %d, current %d", orchestration.ErrAgentStateConflict, transition.Expected, agent.Version)
		}
		if e := orchestration.ValidateAgentTransition(agent.State, transition.Target); e != nil {
			return e
		}
		if isTerminalAgentState(agent.State) && transition.Target == orchestration.AgentQueued {
			var completion completionRecord
			found, e := tx.Get(recordKey("agent-completion", completionKey(agent.ID, agent.CurrentRunID)), &completion)
			if e != nil {
				return e
			}
			if found && !completion.Settled {
				return fmt.Errorf("%w: previous Run result is still settling", orchestration.ErrAgentStateConflict)
			}
		}
		if transition.Result != nil {
			if e := transition.Result.Validate(); e != nil {
				return e
			}
		}
		eventType, e := agentTransitionEvent(transition.Target)
		if e != nil {
			return e
		}
		pending := []orchestration.PendingEvent{{Type: eventType, CausationID: transition.CausationID, CorrelationID: transition.CorrelationID, Payload: orchestration.MustJSONPayload(map[string]any{
			"from": agent.State, "to": transition.Target, "activity": transition.Activity, "run_id": transition.RunID, "reason": transition.Reason, "result": transition.Result,
		})}}
		if transition.Target == orchestration.AgentRunning {
			pending = append([]orchestration.PendingEvent{{Type: orchestration.EventAgentRunAdmitted, CausationID: transition.CausationID, CorrelationID: transition.CorrelationID, Payload: orchestration.MustJSONPayload(map[string]any{"run_id": transition.RunID, "agent_id": agent.ID})}}, pending...)
		}
		if transition.Result != nil && isTerminalAgentState(transition.Target) {
			resultEvents := []orchestration.PendingEvent{{Type: orchestration.EventAgentResultRecorded, CausationID: transition.CausationID, CorrelationID: transition.CorrelationID, Payload: orchestration.MustJSONPayload(map[string]any{"result": transition.Result})}}
			if transition.Target != orchestration.AgentCanceled {
				runType := orchestration.EventAgentRunCompleted
				if transition.Target == orchestration.AgentFailed {
					runType = orchestration.EventAgentRunFailed
				}
				resultEvents = append(resultEvents, orchestration.PendingEvent{Type: runType, CausationID: transition.CausationID, CorrelationID: transition.CorrelationID, Payload: orchestration.MustJSONPayload(map[string]any{"result": transition.Result})})
			}
			pending = append(resultEvents, pending...)
		}
		agent.State, agent.Activity, agent.Result, agent.UpdatedAt = transition.Target, transition.Activity, transition.Result, now
		if transition.RunID != "" {
			agent.CurrentRunID = transition.RunID
		}
		agent.Version++
		if e := putRecord(tx, "agent", string(agent.ID), agent); e != nil {
			return e
		}
		if _, e := appendEvents(tx, orchestration.Stream{Type: orchestration.StreamAgent, ID: string(agent.ID), TeamID: agent.TeamID}, orchestration.AnySequence, pending...); e != nil {
			return e
		}
		if isTerminalAgentState(agent.State) {
			spec, notify, e := orchestration.ResultMessage(agent)
			if e != nil {
				return e
			}
			if notify {
				if _, e := enqueueTx(tx, spec, now); e != nil {
					return e
				}
			}
		}
		result = agent
		return nil
	})
	return
}
func agentTransitionEvent(state orchestration.AgentState) (orchestration.EventType, error) {
	types := map[orchestration.AgentState]orchestration.EventType{
		orchestration.AgentQueued: orchestration.EventAgentQueued, orchestration.AgentRunning: orchestration.EventAgentRunStarted,
		orchestration.AgentWaiting: orchestration.EventAgentWaiting, orchestration.AgentCanceling: orchestration.EventAgentCancelRequested,
		orchestration.AgentCompleted: orchestration.EventAgentCompleted, orchestration.AgentFailed: orchestration.EventAgentFailed,
		orchestration.AgentCanceled: orchestration.EventAgentCanceled,
	}
	if event, ok := types[state]; ok {
		return event, nil
	}
	return "", fmt.Errorf("%w: unsupported Agent target %q", orchestration.ErrInvalidEvent, state)
}

type completionRecord struct {
	Completion orchestration.AgentCompletion
	Result     json.RawMessage
	Checksum   string
	Settled    bool
	CreatedAt  time.Time
	SettledAt  *time.Time
}

func completionKey(agent orchestration.AgentID, run orchestration.RunID) string {
	return string(agent) + "\x00" + string(run)
}
func (store *Store) PrepareAgentCompletion(ctx context.Context, completion orchestration.AgentCompletion) (orchestration.AgentCompletion, error) {
	if completion.AgentID == "" || completion.RunID == "" {
		return orchestration.AgentCompletion{}, fmt.Errorf("%w: completion identity is required", orchestration.ErrInvalidEvent)
	}
	if err := completion.Result.Validate(); err != nil {
		return orchestration.AgentCompletion{}, err
	}
	data, err := json.Marshal(completion.Result)
	if err != nil {
		return orchestration.AgentCompletion{}, err
	}
	record := completionRecord{Completion: completion, Result: data, Checksum: sha256Hex(data), CreatedAt: time.Now().UTC()}
	err = store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		agent, e := agentTx(tx, completion.AgentID)
		if e != nil {
			return e
		}
		if agent.CurrentRunID != completion.RunID {
			return fmt.Errorf("%w: stale completion Run ID", orchestration.ErrAgentStateConflict)
		}
		key := completionKey(completion.AgentID, completion.RunID)
		var existing completionRecord
		found, e := tx.Get(recordKey("agent-completion", key), &existing)
		if e != nil {
			return e
		}
		if found {
			if existing.Checksum != record.Checksum {
				return fmt.Errorf("%w: Agent completion", orchestration.ErrIdempotencyConflict)
			}
			return nil
		}
		return createRecord(tx, "agent-completion", key, record)
	})
	if err != nil {
		return orchestration.AgentCompletion{}, err
	}
	return completion, nil
}
func (store *Store) PendingAgentCompletions(ctx context.Context, team orchestration.TeamID) (result []orchestration.AgentCompletion, err error) {
	err = store.authority.View(ctx, func(tx *ateauthority.Tx) error {
		records, e := listRecords[completionRecord](tx, "agent-completion")
		if e != nil {
			return e
		}
		sort.Slice(records, func(i, j int) bool {
			if records[i].CreatedAt.Equal(records[j].CreatedAt) {
				return records[i].Completion.AgentID < records[j].Completion.AgentID
			}
			return records[i].CreatedAt.Before(records[j].CreatedAt)
		})
		result = nil
		for _, record := range records {
			if record.Settled {
				continue
			}
			agent, e := agentTx(tx, record.Completion.AgentID)
			if e != nil {
				return e
			}
			if agent.TeamID != team {
				continue
			}
			if sha256Hex(record.Result) != record.Checksum {
				return fmt.Errorf("%w: corrupt Agent completion", orchestration.ErrInvalidEvent)
			}
			if e := json.Unmarshal(record.Result, &record.Completion.Result); e != nil {
				return e
			}
			if e := record.Completion.Result.Validate(); e != nil {
				return e
			}
			result = append(result, record.Completion)
		}
		return nil
	})
	return
}
func (store *Store) SettleAgentCompletion(ctx context.Context, agent orchestration.AgentID, run orchestration.RunID) error {
	now := time.Now().UTC()
	return store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		key := completionKey(agent, run)
		record, e := getRecord[completionRecord](tx, "agent-completion", key)
		if e != nil {
			return e
		}
		if record.Settled {
			return nil
		}
		record.Settled, record.SettledAt = true, &now
		return putRecord(tx, "agent-completion", key, record)
	})
}

var _ orchestration.AgentRepository = (*Store)(nil)

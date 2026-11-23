package atestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

func rootThreadTx(tx *ateauthority.Tx, id session.ID) (orchestration.RootThread, error) {
	root, err := getRecord[orchestration.RootThread](tx, "root", string(id))
	if err != nil {
		return root, err
	}
	team, err := getRecord[orchestration.TeamSnapshot](tx, "team", string(root.TeamID))
	if err != nil {
		return root, err
	}
	agent, err := getRecord[orchestration.AgentSnapshot](tx, "agent", string(root.AgentID))
	if err != nil {
		return root, err
	}
	root.TeamState, root.AgentState, root.ProfileRevision = team.State, agent.State, agent.ProfileRevision
	return root, nil
}
func (store *Store) RootThreadBySession(ctx context.Context, id session.ID) (result orchestration.RootThread, err error) {
	if id == "" {
		return result, fmt.Errorf("%w: session ID is required", orchestration.ErrInvalidEvent)
	}
	err = store.authority.View(ctx, func(tx *ateauthority.Tx) error { var e error; result, e = rootThreadTx(tx, id); return e })
	return
}
func (store *Store) EnsureRootThread(ctx context.Context, seed orchestration.RootThreadSeed) (root orchestration.RootThread, created bool, err error) {
	if seed.SessionID == "" {
		return root, false, fmt.Errorf("%w: session ID is required", orchestration.ErrInvalidEvent)
	}
	if seed.ProfileRevision == "" {
		seed.ProfileRevision = "default-v1"
	}
	if seed.TeamID == "" {
		seed.TeamID, err = orchestration.NewTeamID()
		if err != nil {
			return
		}
	}
	if seed.AgentID == "" {
		seed.AgentID, err = orchestration.NewAgentID()
		if err != nil {
			return
		}
	}
	if seed.ThreadID == "" {
		seed.ThreadID, err = orchestration.NewThreadID()
		if err != nil {
			return
		}
	}
	now := time.Now().UTC()
	err = store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		created = false
		existing, e := rootThreadTx(tx, seed.SessionID)
		if e == nil {
			switch existing.TeamState {
			case orchestration.TeamCompleted, orchestration.TeamFailed, orchestration.TeamCanceled:
				team, e := transitionTeamTx(tx, orchestration.TeamTransition{TeamID: existing.TeamID, Expected: orchestration.AnySequence, Target: orchestration.TeamActive, Reason: "session reopened"}, now)
				if e != nil {
					return e
				}
				existing.TeamState = team.State
			}
			root = existing
			return nil
		}
		if !errors.Is(e, orchestration.ErrNotFound) {
			return e
		}
		var raw json.RawMessage
		if found, e := tx.Get(recordKey("root", string(seed.SessionID)), &raw); e != nil {
			return e
		} else if found {
			return fmt.Errorf("%w: dangling root", orchestration.ErrInvalidEvent)
		}
		if e := seed.TeamID.Validate(); e != nil {
			return e
		}
		if e := seed.AgentID.Validate(); e != nil {
			return e
		}
		if e := seed.ThreadID.Validate(); e != nil {
			return e
		}
		root = orchestration.RootThread{TeamID: seed.TeamID, AgentID: seed.AgentID, ThreadID: seed.ThreadID, SessionID: seed.SessionID, TeamState: orchestration.TeamActive, AgentState: orchestration.AgentQueued, ProfileRevision: seed.ProfileRevision, CreatedAt: now}
		agent := orchestration.AgentSnapshot{ID: seed.AgentID, TeamID: seed.TeamID, ThreadID: seed.ThreadID, SessionID: seed.SessionID, State: orchestration.AgentQueued, ContextPolicy: "none", ProfileRevision: seed.ProfileRevision, ProfileSnapshot: seed.ProfileSnapshot, Version: 1, CreatedAt: now, UpdatedAt: now}
		if len(agent.ProfileSnapshot) == 0 {
			agent.ProfileSnapshot = json.RawMessage(`{}`)
		}
		if e := createRecord(tx, "team", string(seed.TeamID), orchestration.TeamSnapshot{ID: seed.TeamID, RootAgentID: seed.AgentID, State: orchestration.TeamActive, Version: 1, UpdatedAt: now}); e != nil {
			return e
		}
		if e := createRecord(tx, "team-created", string(seed.TeamID), now); e != nil {
			return e
		}
		if e := createRecord(tx, "agent", string(seed.AgentID), agent); e != nil {
			return e
		}
		if e := createRecord(tx, "thread-owner", string(seed.ThreadID), seed.AgentID); e != nil {
			return e
		}
		if e := createRecord(tx, "session-thread", string(seed.SessionID), seed.ThreadID); e != nil {
			return e
		}
		if e := createRecord(tx, "root", string(seed.SessionID), root); e != nil {
			return e
		}
		if _, e := appendEvents(tx, orchestration.Stream{Type: orchestration.StreamTeam, ID: string(seed.TeamID), TeamID: seed.TeamID}, orchestration.AnySequence,
			orchestration.PendingEvent{Type: orchestration.EventTeamCreated, Payload: orchestration.MustJSONPayload(map[string]any{"root_agent_id": seed.AgentID, "source": "legacy_session"})}); e != nil {
			return e
		}
		if _, e := appendEvents(tx, orchestration.Stream{Type: orchestration.StreamAgent, ID: string(seed.AgentID), TeamID: seed.TeamID}, orchestration.AnySequence,
			orchestration.PendingEvent{Type: orchestration.EventAgentCreated, Payload: orchestration.MustJSONPayload(map[string]any{"thread_id": seed.ThreadID, "profile_revision": seed.ProfileRevision, "profile_snapshot": seed.ProfileSnapshot})},
			orchestration.PendingEvent{Type: orchestration.EventAgentQueued, ActorAgentID: seed.AgentID, Payload: orchestration.MustJSONPayload(map[string]any{"reason": "root_thread_ready"})}); e != nil {
			return e
		}
		if _, e := appendEvents(tx, orchestration.Stream{Type: orchestration.StreamThread, ID: string(seed.ThreadID), TeamID: seed.TeamID}, orchestration.AnySequence,
			orchestration.PendingEvent{Type: orchestration.EventThreadCreated, Payload: orchestration.MustJSONPayload(map[string]any{"session_id": seed.SessionID, "agent_id": seed.AgentID})}); e != nil {
			return e
		}
		created = true
		return nil
	})
	if err != nil {
		return orchestration.RootThread{}, false, err
	}
	return
}

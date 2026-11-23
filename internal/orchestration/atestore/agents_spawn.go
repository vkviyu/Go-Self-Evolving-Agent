package atestore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

type spawnReceipt struct {
	AgentID orchestration.AgentID
	Hash    string
}

func scopedKey(team orchestration.TeamID, key string) string { return string(team) + "\x00" + key }
func hashSpawnRequest(spec orchestration.SpawnSpec) string {
	workspace := spec.Workspace
	if workspace == orchestration.WorkspaceAuto {
		workspace = ""
	}
	request := struct {
		TeamID          orchestration.TeamID           `json:"team_id"`
		ParentAgentID   orchestration.AgentID          `json:"parent_agent_id"`
		ProfileName     string                         `json:"profile_name"`
		ProfileRevision string                         `json:"profile_revision"`
		TaskID          orchestration.TaskID           `json:"task_id"`
		TaskVersion     uint64                         `json:"task_version,omitempty"`
		InitialPrompt   string                         `json:"initial_prompt"`
		ContextPolicy   string                         `json:"context_policy"`
		ProfileSnapshot json.RawMessage                `json:"profile_snapshot,omitempty"`
		Workspace       orchestration.WorkspaceRequest `json:"workspace,omitempty"`
	}{spec.TeamID, spec.ParentAgentID, spec.ProfileName, spec.ProfileRevision, spec.TaskID, spec.TaskVersion, spec.InitialPrompt, spec.ContextPolicy, spec.ProfileSnapshot, workspace}
	data, _ := json.Marshal(request)
	return sha256Hex(data)
}
func findSpawnTx(tx *ateauthority.Tx, team orchestration.TeamID, key, hash string) (orchestration.AgentSnapshot, bool, error) {
	var receipt spawnReceipt
	found, err := tx.Get(recordKey("spawn-receipt", scopedKey(team, key)), &receipt)
	if err != nil || !found {
		return orchestration.AgentSnapshot{}, false, err
	}
	if hash != "" && receipt.Hash != hash {
		return orchestration.AgentSnapshot{}, false, fmt.Errorf("%w: spawn key %q", orchestration.ErrIdempotencyConflict, key)
	}
	agent, err := agentTx(tx, receipt.AgentID)
	return agent, err == nil, err
}
func (store *Store) FindSpawn(ctx context.Context, team orchestration.TeamID, key string) (result orchestration.AgentSnapshot, found bool, err error) {
	if err = team.Validate(); err != nil {
		return
	}
	if key == "" {
		return
	}
	err = store.authority.View(ctx, func(tx *ateauthority.Tx) error {
		var e error
		result, found, e = findSpawnTx(tx, team, key, "")
		return e
	})
	return
}
func (store *Store) FindSpawnRequest(ctx context.Context, spec orchestration.SpawnSpec) (result orchestration.AgentSnapshot, found bool, err error) {
	if err = spec.TeamID.Validate(); err != nil {
		return
	}
	if spec.IdempotencyKey == "" {
		return
	}
	if spec.Workspace, err = spec.Workspace.Normalize(); err != nil {
		return
	}
	if spec.ContextPolicy == "" {
		spec.ContextPolicy = "summary"
	}
	err = store.authority.View(ctx, func(tx *ateauthority.Tx) error {
		var e error
		result, found, e = findSpawnTx(tx, spec.TeamID, spec.IdempotencyKey, hashSpawnRequest(spec))
		return e
	})
	return
}
func (store *Store) CreateChild(ctx context.Context, spec orchestration.SpawnSpec) (result orchestration.AgentSnapshot, err error) {
	if err = spec.TeamID.Validate(); err != nil {
		return
	}
	if err = spec.ParentAgentID.Validate(); err != nil {
		return
	}
	if spec.ProfileRevision == "" {
		return result, fmt.Errorf("%w: profile revision is required", orchestration.ErrInvalidEvent)
	}
	if len(spec.ProfileSnapshot) > 0 && !json.Valid(spec.ProfileSnapshot) {
		return result, fmt.Errorf("invalid profile snapshot JSON")
	}
	if spec.Workspace, err = spec.Workspace.Normalize(); err != nil {
		return
	}
	if err = spec.WorkspaceMode.Validate(); err != nil {
		return
	}
	if spec.ContextPolicy == "" {
		spec.ContextPolicy = "summary"
	}
	id, err := orchestration.NewAgentID()
	if err != nil {
		return result, err
	}
	thread, err := orchestration.NewThreadID()
	if err != nil {
		return result, err
	}
	now := time.Now().UTC()
	err = store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		if spec.IdempotencyKey != "" {
			existing, found, e := findSpawnTx(tx, spec.TeamID, spec.IdempotencyKey, hashSpawnRequest(spec))
			if e != nil {
				return e
			}
			if found {
				result = existing
				return nil
			}
		}
		parent, e := agentTx(tx, spec.ParentAgentID)
		if e != nil {
			return e
		}
		if parent.TeamID != spec.TeamID {
			return fmt.Errorf("%w: parent Agent belongs to another team", orchestration.ErrInvalidEvent)
		}
		if isTerminalAgentState(parent.State) || parent.State == orchestration.AgentCanceling {
			return fmt.Errorf("%w: parent Agent cannot spawn in state %q", orchestration.ErrInvalidEvent, parent.State)
		}
		if e := requireActiveTeam(tx, spec.TeamID); e != nil {
			return e
		}
		agent := orchestration.AgentSnapshot{ID: id, TeamID: spec.TeamID, ParentAgentID: spec.ParentAgentID, ThreadID: thread, SessionID: session.ID("agent-" + string(thread)), AssignedTaskID: spec.TaskID,
			InitialPrompt: spec.InitialPrompt, ContextPolicy: spec.ContextPolicy, ProfileRevision: spec.ProfileRevision, ProfileSnapshot: spec.ProfileSnapshot, Workspace: spec.Workspace, WorkspaceMode: spec.WorkspaceMode,
			State: orchestration.AgentQueued, Depth: parent.Depth + 1, Version: 1, CreatedAt: now, UpdatedAt: now}
		if e := createRecord(tx, "agent", string(id), agent); e != nil {
			return e
		}
		if e := createRecord(tx, "agent-prompt", string(id), spec.InitialPrompt); e != nil {
			return e
		}
		if e := createRecord(tx, "thread-owner", string(thread), id); e != nil {
			return e
		}
		if e := createRecord(tx, "session-thread", string(agent.SessionID), thread); e != nil {
			return e
		}
		if e := createRecord(tx, "thread-parent", string(thread), parent.ThreadID); e != nil {
			return e
		}
		if spec.TaskID != "" {
			task, e := getRecord[orchestration.TaskSnapshot](tx, "task", string(spec.TaskID))
			if e != nil {
				return e
			}
			if spec.TaskVersion != 0 && task.Version != spec.TaskVersion {
				return fmt.Errorf("%w: dispatch expected task version %d, current %d", orchestration.ErrTaskStateConflict, spec.TaskVersion, task.Version)
			}
			if task.TeamID != spec.TeamID {
				return fmt.Errorf("%w: assigned task belongs to another team", orchestration.ErrInvalidEvent)
			}
			if task.State != orchestration.TaskReady || task.AssigneeAgentID != "" {
				return fmt.Errorf("%w: task is not ready for Agent claim", orchestration.ErrTaskClaimConflict)
			}
			// The task helper performs dependency validation and the claim under
			// this same authority transaction, retaining the spawn actor metadata.
			if _, e := transitionTaskTx(tx, orchestration.TaskTransition{TaskID: spec.TaskID, Expected: task.Version, Target: orchestration.TaskClaimed, AssigneeAgentID: id, ActorAgentID: spec.ParentAgentID, CorrelationID: spec.CorrelationID, Reason: "claimed by spawned Agent"}, now); e != nil {
				return e
			}
		}
		if spec.IdempotencyKey != "" {
			if e := createRecord(tx, "spawn-receipt", scopedKey(spec.TeamID, spec.IdempotencyKey), spawnReceipt{AgentID: id, Hash: hashSpawnRequest(spec)}); e != nil {
				return e
			}
		}
		if _, e := appendEvents(tx, orchestration.Stream{Type: orchestration.StreamAgent, ID: string(id), TeamID: spec.TeamID}, orchestration.AnySequence,
			orchestration.PendingEvent{Type: orchestration.EventAgentCreated, CorrelationID: spec.CorrelationID, IdempotencyKey: spec.IdempotencyKey, Payload: orchestration.MustJSONPayload(map[string]any{
				"parent_agent_id": spec.ParentAgentID, "thread_id": thread, "task_id": spec.TaskID, "profile_revision": spec.ProfileRevision, "profile_snapshot": spec.ProfileSnapshot, "depth": agent.Depth,
				"workspace": spec.Workspace, "workspace_mode": spec.WorkspaceMode, "task_version": spec.TaskVersion})},
			orchestration.PendingEvent{Type: orchestration.EventAgentQueued, CorrelationID: spec.CorrelationID, Payload: orchestration.MustJSONPayload(map[string]any{"reason": "spawned"})}); e != nil {
			return e
		}
		if _, e := appendEvents(tx, orchestration.Stream{Type: orchestration.StreamThread, ID: string(thread), TeamID: spec.TeamID}, orchestration.AnySequence,
			orchestration.PendingEvent{Type: orchestration.EventThreadCreated, CorrelationID: spec.CorrelationID, Payload: orchestration.MustJSONPayload(map[string]any{"session_id": agent.SessionID, "agent_id": id, "parent_thread_id": parent.ThreadID, "context_policy": spec.ContextPolicy})},
			orchestration.PendingEvent{Type: orchestration.EventThreadContextSeeded, CorrelationID: spec.CorrelationID, Payload: orchestration.MustJSONPayload(map[string]any{"initial_prompt": spec.InitialPrompt, "context_policy": spec.ContextPolicy})}); e != nil {
			return e
		}
		result = agent
		return nil
	})
	return
}

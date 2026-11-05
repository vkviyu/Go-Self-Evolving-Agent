package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

func hashSpawnRequest(spec orchestration.SpawnSpec) string {
	// Correlation IDs identify an invocation, but do not change the operation
	// represented by an idempotency key. Keep the hash limited to its inputs.
	// The granted workspace mode is derived from hashed inputs, and an auto
	// request is omitted so keys recorded before it existed still match.
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
	}{
		TeamID: spec.TeamID, ParentAgentID: spec.ParentAgentID, ProfileName: spec.ProfileName,
		ProfileRevision: spec.ProfileRevision, TaskID: spec.TaskID, TaskVersion: spec.TaskVersion, InitialPrompt: spec.InitialPrompt,
		ContextPolicy: spec.ContextPolicy, ProfileSnapshot: spec.ProfileSnapshot, Workspace: workspace,
	}
	encoded, _ := json.Marshal(request)
	return sha256Hex(encoded)
}

func (store *OrchestrationStore) CreateChild(ctx context.Context, spec orchestration.SpawnSpec) (orchestration.AgentSnapshot, error) {
	if err := spec.TeamID.Validate(); err != nil {
		return orchestration.AgentSnapshot{}, fmt.Errorf("invalid spawn team: %w", err)
	}
	if err := spec.ParentAgentID.Validate(); err != nil {
		return orchestration.AgentSnapshot{}, fmt.Errorf("invalid spawn parent: %w", err)
	}
	if spec.ProfileRevision == "" {
		return orchestration.AgentSnapshot{}, fmt.Errorf("%w: profile revision is required", orchestration.ErrInvalidEvent)
	}
	if len(spec.ProfileSnapshot) > 0 && !json.Valid(spec.ProfileSnapshot) {
		return orchestration.AgentSnapshot{}, errors.New("invalid profile snapshot JSON")
	}
	var err error
	if spec.Workspace, err = spec.Workspace.Normalize(); err != nil {
		return orchestration.AgentSnapshot{}, err
	}
	if err := spec.WorkspaceMode.Validate(); err != nil {
		return orchestration.AgentSnapshot{}, err
	}
	if spec.ContextPolicy == "" {
		spec.ContextPolicy = "summary"
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return orchestration.AgentSnapshot{}, fmt.Errorf("begin child Agent creation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if spec.IdempotencyKey != "" {
		var resultID, storedHash string
		err := tx.QueryRowContext(ctx, `SELECT result_id, request_hash FROM idempotency_records WHERE scope = ? AND idempotency_key = ?`,
			"spawn:"+string(spec.TeamID), spec.IdempotencyKey).Scan(&resultID, &storedHash)
		if err == nil {
			if storedHash != "" && storedHash != hashSpawnRequest(spec) {
				return orchestration.AgentSnapshot{}, fmt.Errorf("%w: spawn key %q", orchestration.ErrIdempotencyConflict, spec.IdempotencyKey)
			}
			existing, getErr := agentByID(ctx, tx, orchestration.AgentID(resultID))
			if getErr != nil {
				return orchestration.AgentSnapshot{}, getErr
			}
			if storedHash == "" && !legacySpawnMatches(existing, spec) {
				return orchestration.AgentSnapshot{}, fmt.Errorf("%w: legacy spawn key %q", orchestration.ErrIdempotencyConflict, spec.IdempotencyKey)
			}
			if err := tx.Commit(); err != nil {
				return orchestration.AgentSnapshot{}, fmt.Errorf("commit idempotent child lookup: %w", err)
			}
			return existing, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return orchestration.AgentSnapshot{}, fmt.Errorf("inspect spawn idempotency key: %w", err)
		}
	}
	parent, err := agentByID(ctx, tx, spec.ParentAgentID)
	if err != nil {
		return orchestration.AgentSnapshot{}, fmt.Errorf("load spawn parent: %w", err)
	}
	if parent.TeamID != spec.TeamID {
		return orchestration.AgentSnapshot{}, fmt.Errorf("%w: parent Agent belongs to another team", orchestration.ErrInvalidEvent)
	}
	if parent.State == orchestration.AgentCompleted || parent.State == orchestration.AgentFailed || parent.State == orchestration.AgentCanceled || parent.State == orchestration.AgentCanceling {
		return orchestration.AgentSnapshot{}, fmt.Errorf("%w: parent Agent is not allowed to spawn in state %q", orchestration.ErrInvalidEvent, parent.State)
	}
	// Checked in the creating transaction, so a spawn racing a Team cancel
	// either lands before it and is interrupted, or is rejected.
	if err := requireActiveTeam(ctx, tx, spec.TeamID); err != nil {
		return orchestration.AgentSnapshot{}, err
	}
	agentID, err := orchestration.NewAgentID()
	if err != nil {
		return orchestration.AgentSnapshot{}, err
	}
	threadID, err := orchestration.NewThreadID()
	if err != nil {
		return orchestration.AgentSnapshot{}, err
	}
	sessionID := session.ID("agent-" + string(threadID))
	now := time.Now().UTC()
	depth := parent.Depth + 1
	if _, err := tx.ExecContext(ctx, `INSERT INTO agents(agent_id, team_id, parent_agent_id, thread_id, assigned_task_id, state, activity, profile_revision, profile_snapshot, current_run_id, version, created_at, updated_at, depth, workspace_request, workspace_mode) VALUES(?, ?, ?, ?, ?, ?, '', ?, ?, '', ?, ?, ?, ?, ?, ?)`,
		string(agentID), string(spec.TeamID), string(spec.ParentAgentID), string(threadID), string(spec.TaskID), orchestration.AgentQueued,
		spec.ProfileRevision, []byte(nonNilJSON(spec.ProfileSnapshot)), 1, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), depth,
		string(spec.Workspace), string(spec.WorkspaceMode)); err != nil {
		return orchestration.AgentSnapshot{}, fmt.Errorf("insert child Agent: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_threads(thread_id, session_id, team_id, agent_id, parent_thread_id, context_policy, profile_revision, version, created_at, updated_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(threadID), string(sessionID), string(spec.TeamID), string(agentID), string(parent.ThreadID), spec.ContextPolicy, spec.ProfileRevision, 1,
		now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		return orchestration.AgentSnapshot{}, fmt.Errorf("insert child Thread: %w", err)
	}
	if spec.TaskID != "" {
		if spec.TaskVersion != 0 {
			task, err := taskByID(ctx, tx, spec.TaskID)
			if err != nil {
				return orchestration.AgentSnapshot{}, err
			}
			if task.Version != spec.TaskVersion {
				return orchestration.AgentSnapshot{}, fmt.Errorf("%w: dispatch expected task version %d, current %d", orchestration.ErrTaskStateConflict, spec.TaskVersion, task.Version)
			}
		}
		if err := claimTaskForAgentTx(ctx, tx, spec.TaskID, agentID, spec.ParentAgentID, spec.CorrelationID); err != nil {
			return orchestration.AgentSnapshot{}, fmt.Errorf("claim assigned task: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agent_threads SET initial_prompt = ? WHERE thread_id = ?`, spec.InitialPrompt, string(threadID)); err != nil {
		return orchestration.AgentSnapshot{}, fmt.Errorf("store child initial prompt: %w", err)
	}
	if spec.IdempotencyKey != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO idempotency_records(scope, idempotency_key, result_type, result_id, request_hash, created_at) VALUES(?, ?, ?, ?, ?, ?)`,
			"spawn:"+string(spec.TeamID), spec.IdempotencyKey, "agent", string(agentID), hashSpawnRequest(spec), now.Format(time.RFC3339Nano)); err != nil {
			return orchestration.AgentSnapshot{}, fmt.Errorf("record spawn idempotency key: %w", err)
		}
	}
	if _, err := appendOrchestrationEvents(ctx, tx, orchestration.Stream{Type: orchestration.StreamAgent, ID: string(agentID), TeamID: spec.TeamID}, orchestration.AnySequence,
		orchestration.PendingEvent{Type: orchestration.EventAgentCreated, CorrelationID: spec.CorrelationID, IdempotencyKey: spec.IdempotencyKey, Payload: orchestration.MustJSONPayload(map[string]any{
			"parent_agent_id": spec.ParentAgentID, "thread_id": threadID, "task_id": spec.TaskID, "profile_revision": spec.ProfileRevision, "profile_snapshot": spec.ProfileSnapshot, "depth": depth,
			"workspace": spec.Workspace, "workspace_mode": spec.WorkspaceMode, "task_version": spec.TaskVersion,
		})},
		orchestration.PendingEvent{Type: orchestration.EventAgentQueued, CorrelationID: spec.CorrelationID, Payload: orchestration.MustJSONPayload(map[string]any{"reason": "spawned"})},
	); err != nil {
		return orchestration.AgentSnapshot{}, fmt.Errorf("record child Agent events: %w", err)
	}
	if _, err := appendOrchestrationEvents(ctx, tx, orchestration.Stream{Type: orchestration.StreamThread, ID: string(threadID), TeamID: spec.TeamID}, orchestration.AnySequence,
		orchestration.PendingEvent{Type: orchestration.EventThreadCreated, CorrelationID: spec.CorrelationID, Payload: orchestration.MustJSONPayload(map[string]any{
			"session_id": sessionID, "agent_id": agentID, "parent_thread_id": parent.ThreadID, "context_policy": spec.ContextPolicy,
		})},
		orchestration.PendingEvent{Type: orchestration.EventThreadContextSeeded, CorrelationID: spec.CorrelationID, Payload: orchestration.MustJSONPayload(map[string]any{
			"initial_prompt": spec.InitialPrompt, "context_policy": spec.ContextPolicy,
		})},
	); err != nil {
		return orchestration.AgentSnapshot{}, fmt.Errorf("record child Thread event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return orchestration.AgentSnapshot{}, fmt.Errorf("commit child Agent creation: %w", err)
	}
	return orchestration.AgentSnapshot{
		ID: agentID, TeamID: spec.TeamID, ParentAgentID: spec.ParentAgentID, ThreadID: threadID, SessionID: sessionID,
		AssignedTaskID: spec.TaskID, InitialPrompt: spec.InitialPrompt, ContextPolicy: spec.ContextPolicy, ProfileRevision: spec.ProfileRevision, State: orchestration.AgentQueued,
		Depth: depth, Version: 1, CreatedAt: now, UpdatedAt: now, ProfileSnapshot: append(json.RawMessage(nil), spec.ProfileSnapshot...),
		Workspace: spec.Workspace, WorkspaceMode: spec.WorkspaceMode,
	}, nil
}

func (store *OrchestrationStore) FindSpawn(ctx context.Context, teamID orchestration.TeamID, idempotencyKey string) (orchestration.AgentSnapshot, bool, error) {
	if err := teamID.Validate(); err != nil {
		return orchestration.AgentSnapshot{}, false, fmt.Errorf("invalid spawn team: %w", err)
	}
	if idempotencyKey == "" {
		return orchestration.AgentSnapshot{}, false, nil
	}
	var resultID string
	err := store.db.QueryRowContext(ctx, `SELECT result_id FROM idempotency_records WHERE scope = ? AND idempotency_key = ?`,
		"spawn:"+string(teamID), idempotencyKey).Scan(&resultID)
	if errors.Is(err, sql.ErrNoRows) {
		return orchestration.AgentSnapshot{}, false, nil
	}
	if err != nil {
		return orchestration.AgentSnapshot{}, false, fmt.Errorf("inspect spawn idempotency key: %w", err)
	}
	agent, err := store.GetAgent(ctx, orchestration.AgentID(resultID))
	if err != nil {
		return orchestration.AgentSnapshot{}, false, err
	}
	return agent, true, nil
}

// FindSpawnRequest validates the request hash before the Supervisor applies
// live-agent limits. This lets an idempotent retry return an existing child
// even when the team has since reached its admission limits.
func (store *OrchestrationStore) FindSpawnRequest(ctx context.Context, spec orchestration.SpawnSpec) (orchestration.AgentSnapshot, bool, error) {
	if err := spec.TeamID.Validate(); err != nil {
		return orchestration.AgentSnapshot{}, false, fmt.Errorf("invalid spawn team: %w", err)
	}
	if spec.IdempotencyKey == "" {
		return orchestration.AgentSnapshot{}, false, nil
	}
	var err error
	if spec.Workspace, err = spec.Workspace.Normalize(); err != nil {
		return orchestration.AgentSnapshot{}, false, err
	}
	if spec.ContextPolicy == "" {
		spec.ContextPolicy = "summary"
	}
	var resultID, storedHash string
	err = store.db.QueryRowContext(ctx, `SELECT result_id, request_hash FROM idempotency_records WHERE scope = ? AND idempotency_key = ?`,
		"spawn:"+string(spec.TeamID), spec.IdempotencyKey).Scan(&resultID, &storedHash)
	if errors.Is(err, sql.ErrNoRows) {
		return orchestration.AgentSnapshot{}, false, nil
	}
	if err != nil {
		return orchestration.AgentSnapshot{}, false, fmt.Errorf("inspect spawn request: %w", err)
	}
	if storedHash != "" && storedHash != hashSpawnRequest(spec) {
		return orchestration.AgentSnapshot{}, false, fmt.Errorf("%w: spawn key %q", orchestration.ErrIdempotencyConflict, spec.IdempotencyKey)
	}
	agent, err := store.GetAgent(ctx, orchestration.AgentID(resultID))
	if err != nil {
		return orchestration.AgentSnapshot{}, false, err
	}
	if storedHash == "" && !legacySpawnMatches(agent, spec) {
		return orchestration.AgentSnapshot{}, false, fmt.Errorf("%w: legacy spawn key %q", orchestration.ErrIdempotencyConflict, spec.IdempotencyKey)
	}
	return agent, true, nil
}

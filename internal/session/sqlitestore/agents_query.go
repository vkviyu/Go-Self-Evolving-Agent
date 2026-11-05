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
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

// agentSelect is kept beside its scanner so list and point lookups cannot
// drift in projection column order.
const agentSelect = `
SELECT a.agent_id, a.team_id, a.parent_agent_id, a.thread_id, th.session_id,
       a.assigned_task_id, th.initial_prompt, th.context_policy, a.profile_revision, a.current_run_id,
       a.state, a.activity, a.depth, a.version, a.created_at, a.updated_at, a.result, a.profile_snapshot,
       a.workspace_request, a.workspace_mode
FROM agents a
JOIN agent_threads th ON th.agent_id = a.agent_id
`

func (store *OrchestrationStore) GetAgent(ctx context.Context, agentID orchestration.AgentID) (orchestration.AgentSnapshot, error) {
	if err := agentID.Validate(); err != nil {
		return orchestration.AgentSnapshot{}, fmt.Errorf("invalid Agent ID: %w", err)
	}
	agent, err := agentByID(ctx, store.db, agentID)
	if errors.Is(err, sql.ErrNoRows) {
		return orchestration.AgentSnapshot{}, fmt.Errorf("%w: Agent %s", orchestration.ErrNotFound, agentID)
	}
	return agent, err
}

func (store *OrchestrationStore) ListAgents(ctx context.Context, query orchestration.AgentQuery) ([]orchestration.AgentSnapshot, error) {
	if err := query.TeamID.Validate(); err != nil {
		return nil, fmt.Errorf("invalid Agent query team: %w", err)
	}
	conditions := []string{"a.team_id = ?"}
	arguments := []any{string(query.TeamID)}
	if query.ParentAgentID != "" {
		if err := query.ParentAgentID.Validate(); err != nil {
			return nil, fmt.Errorf("invalid Agent query parent: %w", err)
		}
		conditions = append(conditions, "a.parent_agent_id = ?")
		arguments = append(arguments, string(query.ParentAgentID))
	}
	if len(query.States) > 0 {
		placeholders := make([]string, len(query.States))
		for index, state := range query.States {
			if state == "" {
				return nil, fmt.Errorf("%w: Agent query state is empty", orchestration.ErrInvalidEvent)
			}
			placeholders[index] = "?"
			arguments = append(arguments, string(state))
		}
		conditions = append(conditions, "a.state IN ("+strings.Join(placeholders, ", ")+")")
	}
	queryText := agentSelect + `WHERE ` + strings.Join(conditions, " AND ") + `
ORDER BY a.created_at ASC, a.agent_id ASC`
	rows, err := store.db.QueryContext(ctx, queryText, arguments...)
	if err != nil {
		return nil, fmt.Errorf("list Agents: %w", err)
	}
	defer rows.Close()
	agents := make([]orchestration.AgentSnapshot, 0)
	for rows.Next() {
		agent, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		agents = append(agents, agent)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Agents: %w", err)
	}
	return agents, nil
}

func agentByID(ctx context.Context, query rowQuery, agentID orchestration.AgentID) (orchestration.AgentSnapshot, error) {
	row := query.QueryRowContext(ctx, agentSelect+`WHERE a.agent_id = ?`, string(agentID))
	return scanAgent(row)
}

func scanAgent(scanner rowScanner) (orchestration.AgentSnapshot, error) {
	var agent orchestration.AgentSnapshot
	var agentID, teamID, parentAgentID, threadID, sessionID, taskID, initialPrompt, contextPolicy, currentRunID, state string
	var createdAt, updatedAt, workspaceRequest, workspaceMode string
	var resultPayload []byte
	if err := scanner.Scan(&agentID, &teamID, &parentAgentID, &threadID, &sessionID, &taskID, &initialPrompt, &contextPolicy, &agent.ProfileRevision, &currentRunID, &state, &agent.Activity, &agent.Depth, &agent.Version, &createdAt, &updatedAt, &resultPayload, &agent.ProfileSnapshot, &workspaceRequest, &workspaceMode); err != nil {
		return orchestration.AgentSnapshot{}, err
	}
	agent.ID = orchestration.AgentID(agentID)
	agent.TeamID = orchestration.TeamID(teamID)
	agent.ParentAgentID = orchestration.AgentID(parentAgentID)
	agent.ThreadID = orchestration.ThreadID(threadID)
	agent.SessionID = session.ID(sessionID)
	agent.AssignedTaskID = orchestration.TaskID(taskID)
	agent.InitialPrompt = initialPrompt
	agent.ContextPolicy = contextPolicy
	agent.Workspace = orchestration.WorkspaceRequest(workspaceRequest)
	agent.WorkspaceMode = orchestration.WorkspaceMode(workspaceMode)
	agent.CurrentRunID = orchestration.RunID(currentRunID)
	agent.State = orchestration.AgentState(state)
	if len(resultPayload) > 0 {
		var result orchestration.AgentResult
		if err := json.Unmarshal(resultPayload, &result); err != nil {
			return orchestration.AgentSnapshot{}, fmt.Errorf("decode Agent result: %w", err)
		}
		agent.Result = &result
	}
	var err error
	agent.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return orchestration.AgentSnapshot{}, fmt.Errorf("parse Agent creation time: %w", err)
	}
	agent.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return orchestration.AgentSnapshot{}, fmt.Errorf("parse Agent update time: %w", err)
	}
	return agent, nil
}

package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

// taskSelect is kept beside its scanner so list and point lookups cannot
// drift in projection column order.
const taskSelect = `SELECT task_id, team_id, parent_task_id, title, description, state, priority, assignee_agent_id, required_profile, verification_profile, claim_expires_at, attempt, version, created_at, updated_at FROM tasks `

func (store *OrchestrationStore) GetTask(ctx context.Context, taskID orchestration.TaskID) (orchestration.TaskSnapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := taskID.Validate(); err != nil {
		return orchestration.TaskSnapshot{}, fmt.Errorf("invalid task ID: %w", err)
	}
	task, err := taskByID(ctx, store.db, taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return orchestration.TaskSnapshot{}, fmt.Errorf("%w: task %s", orchestration.ErrNotFound, taskID)
	}
	return task, err
}

func (store *OrchestrationStore) ListTasks(ctx context.Context, query orchestration.TaskQuery) ([]orchestration.TaskSnapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := query.TeamID.Validate(); err != nil {
		return nil, fmt.Errorf("invalid task query team: %w", err)
	}
	conditions := []string{"team_id = ?"}
	args := []any{string(query.TeamID)}
	if query.ParentTaskID != "" {
		if err := query.ParentTaskID.Validate(); err != nil {
			return nil, fmt.Errorf("invalid task query parent: %w", err)
		}
		conditions = append(conditions, "parent_task_id = ?")
		args = append(args, string(query.ParentTaskID))
	}
	if query.AssigneeAgentID != "" {
		if err := query.AssigneeAgentID.Validate(); err != nil {
			return nil, fmt.Errorf("invalid task query assignee: %w", err)
		}
		conditions = append(conditions, "assignee_agent_id = ?")
		args = append(args, string(query.AssigneeAgentID))
	}
	if len(query.States) > 0 {
		placeholders := make([]string, len(query.States))
		for index, state := range query.States {
			if err := validateTaskState(state); err != nil {
				return nil, err
			}
			placeholders[index] = "?"
			args = append(args, string(state))
		}
		conditions = append(conditions, "state IN ("+strings.Join(placeholders, ", ")+")")
	}
	rows, err := store.db.QueryContext(ctx, taskSelect+`WHERE `+strings.Join(conditions, " AND ")+` ORDER BY priority DESC, created_at ASC, task_id ASC`, args...)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()
	tasks := make([]orchestration.TaskSnapshot, 0)
	for rows.Next() {
		task, scanErr := scanTask(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tasks: %w", err)
	}
	return tasks, nil
}

func taskByID(ctx context.Context, query rowQuery, taskID orchestration.TaskID) (orchestration.TaskSnapshot, error) {
	return scanTask(query.QueryRowContext(ctx, taskSelect+`WHERE task_id = ?`, string(taskID)))
}

func scanTask(scanner rowScanner) (orchestration.TaskSnapshot, error) {
	var task orchestration.TaskSnapshot
	var taskID, teamID, parentTaskID, state, assigneeAgentID string
	var createdAt, updatedAt, claimExpiresAt string
	if err := scanner.Scan(&taskID, &teamID, &parentTaskID, &task.Title, &task.Description, &state, &task.Priority, &assigneeAgentID, &task.RequiredProfile, &task.VerificationProfile, &claimExpiresAt, &task.Attempt, &task.Version, &createdAt, &updatedAt); err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	task.ID = orchestration.TaskID(taskID)
	task.TeamID = orchestration.TeamID(teamID)
	task.ParentTaskID = orchestration.TaskID(parentTaskID)
	task.State = orchestration.TaskState(state)
	task.AssigneeAgentID = orchestration.AgentID(assigneeAgentID)
	if claimExpiresAt != "" {
		expiresAt, parseErr := time.Parse(time.RFC3339Nano, claimExpiresAt)
		if parseErr != nil {
			return orchestration.TaskSnapshot{}, fmt.Errorf("parse task claim expiry: %w", parseErr)
		}
		task.ClaimExpiresAt = &expiresAt
	}
	var err error
	task.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return orchestration.TaskSnapshot{}, fmt.Errorf("parse task creation time: %w", err)
	}
	task.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return orchestration.TaskSnapshot{}, fmt.Errorf("parse task update time: %w", err)
	}
	return task, nil
}

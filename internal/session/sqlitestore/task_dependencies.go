package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

func (store *OrchestrationStore) AddDependency(ctx context.Context, taskID, dependencyID orchestration.TaskID, actorAgentID orchestration.AgentID, correlationID string) error {
	return store.changeDependency(ctx, taskID, dependencyID, actorAgentID, correlationID, true)
}

func (store *OrchestrationStore) RemoveDependency(ctx context.Context, taskID, dependencyID orchestration.TaskID, actorAgentID orchestration.AgentID, correlationID string) error {
	return store.changeDependency(ctx, taskID, dependencyID, actorAgentID, correlationID, false)
}

func (store *OrchestrationStore) ListDependencies(ctx context.Context, taskID orchestration.TaskID) ([]orchestration.TaskID, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := taskID.Validate(); err != nil {
		return nil, fmt.Errorf("invalid task ID: %w", err)
	}
	rows, err := store.db.QueryContext(ctx, `SELECT dependency_task_id FROM task_dependencies WHERE task_id = ? ORDER BY dependency_task_id ASC`, string(taskID))
	if err != nil {
		return nil, fmt.Errorf("list task dependencies: %w", err)
	}
	defer rows.Close()
	dependencies := make([]orchestration.TaskID, 0)
	for rows.Next() {
		var dependencyID string
		if err := rows.Scan(&dependencyID); err != nil {
			return nil, fmt.Errorf("scan task dependency: %w", err)
		}
		dependencies = append(dependencies, orchestration.TaskID(dependencyID))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate task dependencies: %w", err)
	}
	return dependencies, nil
}

func (store *OrchestrationStore) changeDependency(ctx context.Context, taskID, dependencyID orchestration.TaskID, actorAgentID orchestration.AgentID, correlationID string, add bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := taskID.Validate(); err != nil {
		return fmt.Errorf("invalid task ID: %w", err)
	}
	if err := dependencyID.Validate(); err != nil {
		return fmt.Errorf("invalid dependency task ID: %w", err)
	}
	if taskID == dependencyID {
		return fmt.Errorf("%w: task cannot depend on itself", orchestration.ErrTaskDependencyCycle)
	}
	if actorAgentID != "" {
		if err := actorAgentID.Validate(); err != nil {
			return fmt.Errorf("invalid task actor: %w", err)
		}
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin task dependency change: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	task, err := taskByID(ctx, tx, taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: task %s", orchestration.ErrNotFound, taskID)
	}
	if err != nil {
		return err
	}
	dependency, err := taskByID(ctx, tx, dependencyID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: dependency task %s", orchestration.ErrNotFound, dependencyID)
	}
	if err != nil {
		return err
	}
	if task.TeamID != dependency.TeamID {
		return fmt.Errorf("%w: dependency task belongs to another team", orchestration.ErrInvalidEvent)
	}
	if isTerminalTask(task.State) {
		return fmt.Errorf("%w: terminal task cannot change dependencies", orchestration.ErrInvalidEvent)
	}
	if actorAgentID != "" {
		if err := agentBelongsToTeam(ctx, tx, actorAgentID, task.TeamID); err != nil {
			return err
		}
	}
	if add {
		var cycle bool
		if err := tx.QueryRowContext(ctx, `WITH RECURSIVE `+taskPlanEdgesCTE+`, reachable(task_id) AS (
    SELECT dependency_id FROM edges WHERE task_id = ?
    UNION
    SELECT e.dependency_id FROM edges e JOIN reachable r ON e.task_id = r.task_id
)
SELECT EXISTS(SELECT 1 FROM reachable WHERE task_id = ?)`, string(dependencyID), string(taskID)).Scan(&cycle); err != nil {
			return fmt.Errorf("check task dependency cycle: %w", err)
		}
		if cycle {
			return fmt.Errorf("%w: %s -> %s", orchestration.ErrTaskDependencyCycle, taskID, dependencyID)
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO task_dependencies(task_id, dependency_task_id, created_at) VALUES(?, ?, ?) ON CONFLICT(task_id, dependency_task_id) DO NOTHING`, string(taskID), string(dependencyID), time.Now().UTC().Format(time.RFC3339Nano))
		if err != nil {
			return fmt.Errorf("add task dependency: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("inspect added dependency: %w", err)
		}
		if changed == 0 {
			return tx.Commit()
		}
	} else {
		result, err := tx.ExecContext(ctx, `DELETE FROM task_dependencies WHERE task_id = ? AND dependency_task_id = ?`, string(taskID), string(dependencyID))
		if err != nil {
			return fmt.Errorf("remove task dependency: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("inspect removed dependency: %w", err)
		}
		if changed == 0 {
			return tx.Commit()
		}
	}
	stateFrom := task.State
	stateTo := task.State
	if add && (task.State == orchestration.TaskDraft || task.State == orchestration.TaskReady) {
		complete, checkErr := dependenciesComplete(ctx, tx, task.ID)
		if checkErr != nil {
			return checkErr
		}
		if !complete {
			stateTo = orchestration.TaskBlocked
		}
	}
	if !add && task.State == orchestration.TaskBlocked {
		complete, checkErr := dependenciesComplete(ctx, tx, task.ID)
		if checkErr != nil {
			return checkErr
		}
		if complete {
			stateTo = orchestration.TaskReady
		}
	}
	stateChanged := stateTo != stateFrom
	if stateChanged {
		now := time.Now().UTC()
		if err := execCAS(ctx, tx, orchestration.ErrTaskStateConflict, `UPDATE tasks SET state = ?, assignee_agent_id = '', version = version + 1, updated_at = ? WHERE task_id = ? AND version = ?`,
			string(stateTo), now.Format(time.RFC3339Nano), string(task.ID), task.Version); err != nil {
			return fmt.Errorf("update dependency readiness: %w", err)
		}
	}
	streamSequence, err := currentOrchestrationSequence(ctx, tx, orchestration.StreamTask, string(taskID))
	if err != nil {
		return err
	}
	eventType := orchestration.EventTaskDependencyAdded
	if !add {
		eventType = orchestration.EventTaskDependencyRemoved
	}
	pendingEvents := []orchestration.PendingEvent{{Type: eventType, ActorAgentID: actorAgentID, CorrelationID: correlationID, Payload: orchestration.MustJSONPayload(map[string]any{
		"task_id": taskID, "dependency_task_id": dependencyID,
	})}}
	if stateChanged {
		pendingEvents = append(pendingEvents, orchestration.PendingEvent{Type: orchestration.EventTaskUpdated, ActorAgentID: actorAgentID, CorrelationID: correlationID, Payload: orchestration.MustJSONPayload(map[string]any{
			"from": stateFrom, "to": stateTo, "assignee_agent_id": "", "reason": "dependency readiness",
		})})
	}
	if _, err := appendOrchestrationEvents(ctx, tx, orchestration.Stream{Type: orchestration.StreamTask, ID: string(taskID), TeamID: task.TeamID}, streamSequence, pendingEvents...); err != nil {
		return fmt.Errorf("record task dependency change: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit task dependency change: %w", err)
	}
	return nil
}

func unblockDependentTasksTx(ctx context.Context, tx *sql.Tx, dependencyID orchestration.TaskID, actorAgentID orchestration.AgentID, correlationID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT dependent.task_id, dependent.team_id, dependent.version
FROM task_dependencies
JOIN tasks dependent ON dependent.task_id = task_dependencies.task_id
WHERE task_dependencies.dependency_task_id = ? AND dependent.state = ?
ORDER BY dependent.task_id ASC`, string(dependencyID), string(orchestration.TaskBlocked))
	if err != nil {
		return fmt.Errorf("list blocked dependents: %w", err)
	}
	type dependentTask struct {
		id      orchestration.TaskID
		teamID  orchestration.TeamID
		version uint64
	}
	dependents := make([]dependentTask, 0)
	for rows.Next() {
		var task dependentTask
		if err := rows.Scan(&task.id, &task.teamID, &task.version); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan blocked dependent: %w", err)
		}
		dependents = append(dependents, task)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate blocked dependents: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close blocked dependents: %w", err)
	}
	for _, dependent := range dependents {
		complete, err := dependenciesComplete(ctx, tx, dependent.id)
		if err != nil {
			return err
		}
		if !complete {
			continue
		}
		now := time.Now().UTC()
		if err := execCAS(ctx, tx, orchestration.ErrTaskStateConflict, `UPDATE tasks SET state = ?, version = version + 1, updated_at = ? WHERE task_id = ? AND version = ? AND state = ?`,
			string(orchestration.TaskReady), now.Format(time.RFC3339Nano), string(dependent.id), dependent.version, string(orchestration.TaskBlocked)); err != nil {
			return fmt.Errorf("unlock dependent task %s: %w", dependent.id, err)
		}
		sequence, err := currentOrchestrationSequence(ctx, tx, orchestration.StreamTask, string(dependent.id))
		if err != nil {
			return err
		}
		if _, err := appendOrchestrationEvents(ctx, tx, orchestration.Stream{Type: orchestration.StreamTask, ID: string(dependent.id), TeamID: dependent.teamID}, sequence,
			orchestration.PendingEvent{Type: orchestration.EventTaskUpdated, ActorAgentID: actorAgentID, CorrelationID: correlationID, Payload: orchestration.MustJSONPayload(map[string]any{
				"from": orchestration.TaskBlocked, "to": orchestration.TaskReady, "assignee_agent_id": "", "reason": "dependencies completed", "dependency_task_id": dependencyID,
			})},
		); err != nil {
			return fmt.Errorf("record dependent task unlock: %w", err)
		}
	}
	return nil
}

func dependenciesComplete(ctx context.Context, query rowQuery, taskID orchestration.TaskID) (bool, error) {
	var incomplete int
	if err := query.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_dependencies d JOIN tasks dependency ON dependency.task_id = d.dependency_task_id WHERE d.task_id = ? AND dependency.state <> ?`, string(taskID), string(orchestration.TaskCompleted)).Scan(&incomplete); err != nil {
		return false, fmt.Errorf("check task dependencies: %w", err)
	}
	return incomplete == 0, nil
}

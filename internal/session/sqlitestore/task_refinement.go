package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

// taskPlanEdgesCTE keeps dependency and refinement cycle checks consistent.
// ParentTaskID by itself is a grouping relation, not a completion dependency.
const taskPlanEdgesCTE = `edges(task_id, dependency_id) AS (SELECT task_id, dependency_task_id FROM task_dependencies UNION SELECT parent_task_id, child_task_id FROM task_refinement_members)`

func hashTaskRefinement(spec orchestration.TaskRefinementSpec) string {
	request := spec
	request.CorrelationID, request.IdempotencyKey = "", ""
	request.Children = slices.Clone(spec.Children)
	request.Dependencies = slices.Clone(spec.Dependencies)
	slices.SortFunc(request.Children, func(a, b orchestration.TaskRefinementChild) int { return compareRefinementKey(a.Key, b.Key) })
	slices.SortFunc(request.Dependencies, func(a, b orchestration.TaskRefinementDependency) int {
		left, _ := json.Marshal(a)
		right, _ := json.Marshal(b)
		return compareRefinementKey(string(left), string(right))
	})
	encoded, _ := json.Marshal(request)
	return sha256Hex(encoded)
}

func compareRefinementKey(a, b string) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

// RefineTasks appends children and their dependency edges as one command.
// Existing execution contracts remain unchanged; the parent's version fences
// concurrent plan edits and completion. A committed command replays even when
// its parent has since completed or its original expected version is stale.
func (store *OrchestrationStore) RefineTasks(ctx context.Context, spec orchestration.TaskRefinementSpec) (orchestration.TaskRefinementResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := spec.Validate(); err != nil {
		return orchestration.TaskRefinementResult{}, err
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return orchestration.TaskRefinementResult{}, fmt.Errorf("begin task refinement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	scope, hash := "task-refinement:"+string(spec.TeamID), hashTaskRefinement(spec)
	var recordedIDs, recordedHash, resultType string
	err = tx.QueryRowContext(ctx, `SELECT result_id, request_hash, result_type FROM idempotency_records WHERE scope = ? AND idempotency_key = ?`, scope, spec.IdempotencyKey).Scan(&recordedIDs, &recordedHash, &resultType)
	if err == nil {
		if recordedHash != hash || resultType != "task-refinement-v1" {
			return orchestration.TaskRefinementResult{}, fmt.Errorf("%w: task refinement key %q", orchestration.ErrIdempotencyConflict, spec.IdempotencyKey)
		}
		var ids map[string]orchestration.TaskID
		if err := json.Unmarshal([]byte(recordedIDs), &ids); err != nil {
			return orchestration.TaskRefinementResult{}, fmt.Errorf("decode task refinement receipt: %w", err)
		}
		if len(ids) != len(spec.Children) {
			return orchestration.TaskRefinementResult{}, fmt.Errorf("%w: invalid task refinement receipt", orchestration.ErrInvalidEvent)
		}
		for _, child := range spec.Children {
			if ids[child.Key] == "" {
				return orchestration.TaskRefinementResult{}, fmt.Errorf("%w: missing task refinement child %q", orchestration.ErrInvalidEvent, child.Key)
			}
		}
		result, err := loadRefinementResult(ctx, tx, spec.TeamID, spec.ParentTaskID, ids)
		if err != nil {
			return orchestration.TaskRefinementResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return orchestration.TaskRefinementResult{}, fmt.Errorf("commit task refinement replay: %w", err)
		}
		return result, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return orchestration.TaskRefinementResult{}, fmt.Errorf("inspect task refinement receipt: %w", err)
	}
	if err := requireActiveTeam(ctx, tx, spec.TeamID); err != nil {
		return orchestration.TaskRefinementResult{}, err
	}
	parent, err := taskByID(ctx, tx, spec.ParentTaskID)
	if errors.Is(err, sql.ErrNoRows) {
		return orchestration.TaskRefinementResult{}, fmt.Errorf("%w: parent task %s", orchestration.ErrNotFound, spec.ParentTaskID)
	}
	if err != nil {
		return orchestration.TaskRefinementResult{}, err
	}
	if parent.TeamID != spec.TeamID {
		return orchestration.TaskRefinementResult{}, fmt.Errorf("%w: parent task belongs to another team", orchestration.ErrInvalidEvent)
	}
	if parent.Version != spec.Expected {
		return orchestration.TaskRefinementResult{}, fmt.Errorf("%w: expected %d, current %d", orchestration.ErrTaskStateConflict, spec.Expected, parent.Version)
	}
	switch parent.State {
	case orchestration.TaskDraft, orchestration.TaskBlocked, orchestration.TaskReady, orchestration.TaskClaimed, orchestration.TaskInProgress:
	default:
		return orchestration.TaskRefinementResult{}, fmt.Errorf("%w: parent task %s is %s", orchestration.ErrTaskRefinementInvalid, parent.ID, parent.State)
	}
	if err := authorizeTaskPlanActorTx(ctx, tx, parent, spec.ActorAgentID); err != nil {
		return orchestration.TaskRefinementResult{}, err
	}
	ids := make(map[string]orchestration.TaskID, len(spec.Children))
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, child := range spec.Children {
		id, err := orchestration.NewTaskID()
		if err != nil {
			return orchestration.TaskRefinementResult{}, err
		}
		ids[child.Key] = id
		if _, err := tx.ExecContext(ctx, `INSERT INTO tasks(task_id, team_id, parent_task_id, title, description, state, priority, assignee_agent_id, required_profile, verification_profile, claim_expires_at, attempt, version, created_at, updated_at) VALUES(?, ?, ?, ?, ?, ?, ?, '', ?, ?, '', 0, 1, ?, ?)`, string(id), string(spec.TeamID), string(parent.ID), child.Title, child.Description, orchestration.TaskDraft, child.Priority, child.RequiredProfile, child.VerificationProfile, now, now); err != nil {
			return orchestration.TaskRefinementResult{}, fmt.Errorf("insert refinement child %q: %w", child.Key, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_refinement_members(parent_task_id, child_task_id, created_at) VALUES(?, ?, ?)`, string(parent.ID), string(id), now); err != nil {
			return orchestration.TaskRefinementResult{}, fmt.Errorf("record refinement membership %q: %w", child.Key, err)
		}
		if _, err := appendOrchestrationEvents(ctx, tx, orchestration.Stream{Type: orchestration.StreamTask, ID: string(id), TeamID: spec.TeamID}, 0,
			orchestration.PendingEvent{Type: orchestration.EventTaskCreated, ActorAgentID: spec.ActorAgentID, CorrelationID: spec.CorrelationID, IdempotencyKey: spec.IdempotencyKey, Payload: orchestration.MustJSONPayload(map[string]any{"parent_task_id": parent.ID, "title": child.Title, "description": child.Description, "priority": child.Priority, "required_profile": child.RequiredProfile, "verification_profile": child.VerificationProfile, "refinement_key": child.Key})}); err != nil {
			return orchestration.TaskRefinementResult{}, fmt.Errorf("record refinement child: %w", err)
		}
	}
	for _, edge := range spec.Dependencies {
		dependencyID := edge.DependencyTaskID
		if edge.DependencyChildKey != "" {
			dependencyID = ids[edge.DependencyChildKey]
		}
		if err := addRefinementDependency(ctx, tx, spec, ids[edge.ChildKey], dependencyID, now); err != nil {
			return orchestration.TaskRefinementResult{}, err
		}
	}
	if err := execCAS(ctx, tx, orchestration.ErrTaskStateConflict, `UPDATE tasks SET version = version + 1, updated_at = ? WHERE task_id = ? AND version = ?`, now, string(parent.ID), parent.Version); err != nil {
		return orchestration.TaskRefinementResult{}, fmt.Errorf("advance refined parent: %w", err)
	}
	sequence, err := currentOrchestrationSequence(ctx, tx, orchestration.StreamTask, string(parent.ID))
	if err != nil {
		return orchestration.TaskRefinementResult{}, err
	}
	if _, err := appendOrchestrationEvents(ctx, tx, orchestration.Stream{Type: orchestration.StreamTask, ID: string(parent.ID), TeamID: spec.TeamID}, sequence,
		orchestration.PendingEvent{Type: orchestration.EventTaskUpdated, ActorAgentID: spec.ActorAgentID, CorrelationID: spec.CorrelationID, IdempotencyKey: spec.IdempotencyKey, Payload: orchestration.MustJSONPayload(map[string]any{"reason": "task refined", "children": ids, "version": parent.Version + 1})}); err != nil {
		return orchestration.TaskRefinementResult{}, fmt.Errorf("record parent refinement: %w", err)
	}
	encodedIDs, err := json.Marshal(ids)
	if err != nil {
		return orchestration.TaskRefinementResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO idempotency_records(scope, idempotency_key, result_type, result_id, request_hash, created_at) VALUES(?, ?, ?, ?, ?, ?)`, scope, spec.IdempotencyKey, "task-refinement-v1", string(encodedIDs), hash, now); err != nil {
		return orchestration.TaskRefinementResult{}, fmt.Errorf("record task refinement receipt: %w", err)
	}
	result, err := loadRefinementResult(ctx, tx, spec.TeamID, parent.ID, ids)
	if err != nil {
		return orchestration.TaskRefinementResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return orchestration.TaskRefinementResult{}, fmt.Errorf("commit task refinement: %w", err)
	}
	return result, nil
}

func addRefinementDependency(ctx context.Context, tx *sql.Tx, spec orchestration.TaskRefinementSpec, childID, dependencyID orchestration.TaskID, now string) error {
	dependency, err := taskByID(ctx, tx, dependencyID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: dependency task %s", orchestration.ErrNotFound, dependencyID)
	}
	if err != nil {
		return err
	}
	if dependency.TeamID != spec.TeamID {
		return fmt.Errorf("%w: dependency task belongs to another team", orchestration.ErrInvalidEvent)
	}
	if childID == dependencyID {
		return fmt.Errorf("%w: task cannot depend on itself", orchestration.ErrTaskDependencyCycle)
	}
	// A refined parent waits for its refinement children. Include those edges in
	// cycle detection so depending on the parent (or on a task waiting for it)
	// cannot deadlock an otherwise acyclic explicit dependency graph.
	var cycle bool
	if err := tx.QueryRowContext(ctx, `WITH RECURSIVE `+taskPlanEdgesCTE+`, reachable(task_id) AS (SELECT ? UNION SELECT e.dependency_id FROM edges e JOIN reachable r ON e.task_id = r.task_id) SELECT EXISTS(SELECT 1 FROM reachable WHERE task_id = ?)`, string(dependencyID), string(childID)).Scan(&cycle); err != nil {
		return fmt.Errorf("check refinement cycle: %w", err)
	}
	if cycle {
		return fmt.Errorf("%w: %s -> %s", orchestration.ErrTaskDependencyCycle, childID, dependencyID)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_dependencies(task_id, dependency_task_id, created_at) VALUES(?, ?, ?)`, string(childID), string(dependencyID), now); err != nil {
		return fmt.Errorf("insert refinement dependency: %w", err)
	}
	sequence, err := currentOrchestrationSequence(ctx, tx, orchestration.StreamTask, string(childID))
	if err != nil {
		return err
	}
	_, err = appendOrchestrationEvents(ctx, tx, orchestration.Stream{Type: orchestration.StreamTask, ID: string(childID), TeamID: spec.TeamID}, sequence,
		orchestration.PendingEvent{Type: orchestration.EventTaskDependencyAdded, ActorAgentID: spec.ActorAgentID, CorrelationID: spec.CorrelationID, Payload: orchestration.MustJSONPayload(map[string]any{"task_id": childID, "dependency_task_id": dependencyID, "refinement": true})})
	return err
}

func loadRefinementResult(ctx context.Context, query rowQuery, teamID orchestration.TeamID, parentID orchestration.TaskID, ids map[string]orchestration.TaskID) (orchestration.TaskRefinementResult, error) {
	result := orchestration.TaskRefinementResult{ParentTaskID: parentID, Children: make(map[string]orchestration.TaskSnapshot, len(ids))}
	for key, id := range ids {
		task, err := taskByID(ctx, query, id)
		if err != nil {
			return orchestration.TaskRefinementResult{}, fmt.Errorf("load refinement child %q: %w", key, err)
		}
		if task.TeamID != teamID || task.ParentTaskID != parentID {
			return orchestration.TaskRefinementResult{}, fmt.Errorf("%w: refinement receipt child differs from parent", orchestration.ErrInvalidEvent)
		}
		var member bool
		if err := query.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM task_refinement_members WHERE parent_task_id = ? AND child_task_id = ?)`, string(parentID), string(id)).Scan(&member); err != nil {
			return orchestration.TaskRefinementResult{}, fmt.Errorf("load refinement membership: %w", err)
		}
		if !member {
			return orchestration.TaskRefinementResult{}, fmt.Errorf("%w: refinement receipt child is not a member", orchestration.ErrInvalidEvent)
		}
		result.Children[key] = task
	}
	return result, nil
}

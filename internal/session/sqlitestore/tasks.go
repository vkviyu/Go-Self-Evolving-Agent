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
)

func hashTaskRequest(spec orchestration.TaskSpec) string {
	request := struct {
		TeamID              orchestration.TeamID  `json:"team_id"`
		ActorAgentID        orchestration.AgentID `json:"actor_agent_id"`
		ParentTaskID        orchestration.TaskID  `json:"parent_task_id"`
		Title               string                `json:"title"`
		Description         string                `json:"description"`
		Priority            int                   `json:"priority"`
		RequiredProfile     string                `json:"required_profile"`
		VerificationProfile string                `json:"verification_profile"`
	}{
		TeamID: spec.TeamID, ActorAgentID: spec.ActorAgentID, ParentTaskID: spec.ParentTaskID, Title: spec.Title,
		Description: spec.Description, Priority: spec.Priority, RequiredProfile: spec.RequiredProfile, VerificationProfile: spec.VerificationProfile,
	}
	encoded, _ := json.Marshal(request)
	return sha256Hex(encoded)
}

func (store *OrchestrationStore) CreateTask(ctx context.Context, spec orchestration.TaskSpec) (orchestration.TaskSnapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := spec.Validate(); err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return orchestration.TaskSnapshot{}, fmt.Errorf("begin task creation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	requestHash := hashTaskRequest(spec)
	if spec.IdempotencyKey != "" {
		var resultID, storedHash string
		err := tx.QueryRowContext(ctx, `SELECT result_id, request_hash FROM idempotency_records WHERE scope = ? AND idempotency_key = ?`,
			"task:"+string(spec.TeamID), spec.IdempotencyKey).Scan(&resultID, &storedHash)
		if err == nil {
			if storedHash != "" && storedHash != requestHash {
				return orchestration.TaskSnapshot{}, fmt.Errorf("%w: task key %q", orchestration.ErrIdempotencyConflict, spec.IdempotencyKey)
			}
			existing, getErr := taskByID(ctx, tx, orchestration.TaskID(resultID))
			if getErr != nil {
				return orchestration.TaskSnapshot{}, getErr
			}
			if storedHash == "" {
				matches, matchErr := legacyTaskMatches(ctx, tx, existing, spec)
				if matchErr != nil {
					return orchestration.TaskSnapshot{}, matchErr
				}
				if !matches {
					return orchestration.TaskSnapshot{}, fmt.Errorf("%w: legacy task key %q", orchestration.ErrIdempotencyConflict, spec.IdempotencyKey)
				}
			}
			if err := tx.Commit(); err != nil {
				return orchestration.TaskSnapshot{}, fmt.Errorf("commit idempotent task lookup: %w", err)
			}
			return existing, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return orchestration.TaskSnapshot{}, fmt.Errorf("inspect task idempotency key: %w", err)
		}
	}
	if spec.ParentTaskID != "" {
		parent, parentErr := taskByID(ctx, tx, spec.ParentTaskID)
		if errors.Is(parentErr, sql.ErrNoRows) {
			return orchestration.TaskSnapshot{}, fmt.Errorf("%w: parent task %s", orchestration.ErrNotFound, spec.ParentTaskID)
		}
		if parentErr != nil {
			return orchestration.TaskSnapshot{}, fmt.Errorf("load parent task: %w", parentErr)
		}
		if parent.TeamID != spec.TeamID {
			return orchestration.TaskSnapshot{}, fmt.Errorf("%w: parent task belongs to another team", orchestration.ErrInvalidEvent)
		}
	}
	if spec.ActorAgentID != "" {
		if err := agentBelongsToTeam(ctx, tx, spec.ActorAgentID, spec.TeamID); err != nil {
			return orchestration.TaskSnapshot{}, err
		}
	}
	taskID, err := orchestration.NewTaskID()
	if err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO tasks(task_id, team_id, parent_task_id, title, description, state, priority, assignee_agent_id, required_profile, verification_profile, claim_expires_at, attempt, version, created_at, updated_at) VALUES(?, ?, ?, ?, ?, ?, ?, '', ?, ?, '', 0, 1, ?, ?)`,
		string(taskID), string(spec.TeamID), string(spec.ParentTaskID), spec.Title, spec.Description, orchestration.TaskDraft, spec.Priority, spec.RequiredProfile, spec.VerificationProfile,
		now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		return orchestration.TaskSnapshot{}, fmt.Errorf("insert task: %w", err)
	}
	if spec.IdempotencyKey != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO idempotency_records(scope, idempotency_key, result_type, result_id, request_hash, created_at) VALUES(?, ?, ?, ?, ?, ?)`,
			"task:"+string(spec.TeamID), spec.IdempotencyKey, "task", string(taskID), requestHash, now.Format(time.RFC3339Nano)); err != nil {
			return orchestration.TaskSnapshot{}, fmt.Errorf("record task idempotency key: %w", err)
		}
	}
	if _, err := appendOrchestrationEvents(ctx, tx, orchestration.Stream{Type: orchestration.StreamTask, ID: string(taskID), TeamID: spec.TeamID}, orchestration.AnySequence,
		orchestration.PendingEvent{Type: orchestration.EventTaskCreated, ActorAgentID: spec.ActorAgentID, CorrelationID: spec.CorrelationID, IdempotencyKey: spec.IdempotencyKey, Payload: orchestration.MustJSONPayload(map[string]any{
			"parent_task_id": spec.ParentTaskID, "title": spec.Title, "description": spec.Description, "priority": spec.Priority, "required_profile": spec.RequiredProfile, "verification_profile": spec.VerificationProfile,
		})},
	); err != nil {
		return orchestration.TaskSnapshot{}, fmt.Errorf("record task creation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return orchestration.TaskSnapshot{}, fmt.Errorf("commit task creation: %w", err)
	}
	return orchestration.TaskSnapshot{
		ID: taskID, TeamID: spec.TeamID, ParentTaskID: spec.ParentTaskID, Title: spec.Title, Description: spec.Description,
		State: orchestration.TaskDraft, Priority: spec.Priority, RequiredProfile: spec.RequiredProfile, VerificationProfile: spec.VerificationProfile, Version: 1, CreatedAt: now, UpdatedAt: now,
	}, nil
}

func (store *OrchestrationStore) TransitionTask(ctx context.Context, transition orchestration.TaskTransition) (orchestration.TaskSnapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := transition.TaskID.Validate(); err != nil {
		return orchestration.TaskSnapshot{}, fmt.Errorf("invalid task transition ID: %w", err)
	}
	if err := validateTaskState(transition.Target); err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	if transition.AssigneeAgentID != "" {
		if err := transition.AssigneeAgentID.Validate(); err != nil {
			return orchestration.TaskSnapshot{}, fmt.Errorf("invalid task assignee: %w", err)
		}
	}
	if transition.ActorAgentID != "" {
		if err := transition.ActorAgentID.Validate(); err != nil {
			return orchestration.TaskSnapshot{}, fmt.Errorf("invalid task actor: %w", err)
		}
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return orchestration.TaskSnapshot{}, fmt.Errorf("begin task transition: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	task, err := taskByID(ctx, tx, transition.TaskID)
	if errors.Is(err, sql.ErrNoRows) {
		return orchestration.TaskSnapshot{}, fmt.Errorf("%w: task %s", orchestration.ErrNotFound, transition.TaskID)
	}
	if err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	if transition.Expected != orchestration.AnySequence && transition.Expected != task.Version {
		return orchestration.TaskSnapshot{}, fmt.Errorf("%w: expected %d, current %d", orchestration.ErrTaskStateConflict, transition.Expected, task.Version)
	}
	if transition.Target == orchestration.TaskVerification || transition.Target == orchestration.TaskAwaitingMerge || transition.Target == orchestration.TaskCompleted {
		var unfinished bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM task_refinement_members m JOIN tasks child ON child.task_id = m.child_task_id WHERE m.parent_task_id = ? AND child.state <> 'completed')`, string(task.ID)).Scan(&unfinished); err != nil {
			return orchestration.TaskSnapshot{}, err
		}
		if unfinished {
			return orchestration.TaskSnapshot{}, fmt.Errorf("%w: refinement children of task %s are not completed", orchestration.ErrTaskNotReady, task.ID)
		}
	}
	if transition.Target == orchestration.TaskCompleted && task.VerificationProfile != "" {
		if task.State == orchestration.TaskInProgress {
			return orchestration.TaskSnapshot{}, fmt.Errorf("%w: task %s requires profile %q", orchestration.ErrTaskVerificationRequired, task.ID, task.VerificationProfile)
		}
		if task.State == orchestration.TaskVerification && (transition.ActorAgentID == "" || transition.ActorAgentID == task.AssigneeAgentID) {
			return orchestration.TaskSnapshot{}, fmt.Errorf("%w: worker %s cannot self-review task %s", orchestration.ErrTaskReviewerRequired, task.AssigneeAgentID, task.ID)
		}
	}
	// Waiting for a merge passes the same review gate as completion.
	if transition.Target == orchestration.TaskAwaitingMerge && task.VerificationProfile != "" {
		if task.State == orchestration.TaskInProgress {
			return orchestration.TaskSnapshot{}, fmt.Errorf("%w: task %s requires profile %q", orchestration.ErrTaskVerificationRequired, task.ID, task.VerificationProfile)
		}
		if transition.ActorAgentID == "" || transition.ActorAgentID == task.AssigneeAgentID {
			return orchestration.TaskSnapshot{}, fmt.Errorf("%w: worker %s cannot self-review task %s", orchestration.ErrTaskReviewerRequired, task.AssigneeAgentID, task.ID)
		}
	}
	if transition.Target == orchestration.TaskAwaitingMerge || transition.Target == orchestration.TaskCompleted {
		open, checkErr := openMerges(ctx, tx, task.ID)
		if checkErr != nil {
			return orchestration.TaskSnapshot{}, checkErr
		}
		if transition.Target == orchestration.TaskAwaitingMerge && open == 0 {
			return orchestration.TaskSnapshot{}, fmt.Errorf("%w: task %s has no queued merge", orchestration.ErrInvalidEvent, task.ID)
		}
		if transition.Target == orchestration.TaskCompleted && open > 0 {
			return orchestration.TaskSnapshot{}, fmt.Errorf("%w: task %s still has %d unmerged merge request(s)", orchestration.ErrInvalidEvent, task.ID, open)
		}
	}
	if (transition.Target == orchestration.TaskClaimed || transition.Target == orchestration.TaskInProgress) && task.State == orchestration.TaskBlocked {
		return orchestration.TaskSnapshot{}, fmt.Errorf("%w: task %s", orchestration.ErrTaskNotReady, task.ID)
	}
	if transition.Target == orchestration.TaskClaimed && task.AssigneeAgentID != "" && task.AssigneeAgentID != transition.AssigneeAgentID {
		return orchestration.TaskSnapshot{}, fmt.Errorf("%w: task %s is assigned to %s", orchestration.ErrTaskClaimConflict, task.ID, task.AssigneeAgentID)
	}
	if err := orchestration.ValidateTaskTransition(task.State, transition.Target); err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	assignee := task.AssigneeAgentID
	if transition.AssigneeAgentID != "" {
		assignee = transition.AssigneeAgentID
	}
	if transition.Target == orchestration.TaskReady {
		// Verification is submitted by the Supervisor-authorized reviewer, not
		// the worker that owns the claim. Reviewer identity is checked at that
		// control-plane boundary; this store still checks Team membership.
		if task.State != orchestration.TaskVerification && transition.ActorAgentID != "" && task.AssigneeAgentID != "" && task.AssigneeAgentID != transition.ActorAgentID {
			return orchestration.TaskSnapshot{}, fmt.Errorf("%w: task is assigned to %s", orchestration.ErrInvalidEvent, task.AssigneeAgentID)
		}
		if task.State != orchestration.TaskClaimed && task.State != orchestration.TaskInProgress && task.State != orchestration.TaskVerification {
			complete, checkErr := dependenciesComplete(ctx, tx, task.ID)
			if checkErr != nil {
				return orchestration.TaskSnapshot{}, checkErr
			}
			if !complete {
				return orchestration.TaskSnapshot{}, fmt.Errorf("%w: task %s", orchestration.ErrTaskNotReady, task.ID)
			}
		}
		assignee = ""
	}
	if transition.Target == orchestration.TaskBlocked {
		assignee = ""
	}
	if transition.Target == orchestration.TaskClaimed || transition.Target == orchestration.TaskInProgress {
		if assignee == "" {
			return orchestration.TaskSnapshot{}, fmt.Errorf("%w: task %s requires an assignee", orchestration.ErrInvalidEvent, transition.Target)
		}
		if err := agentBelongsToTeam(ctx, tx, assignee, task.TeamID); err != nil {
			return orchestration.TaskSnapshot{}, err
		}
		complete, checkErr := dependenciesComplete(ctx, tx, task.ID)
		if checkErr != nil {
			return orchestration.TaskSnapshot{}, checkErr
		}
		if !complete {
			return orchestration.TaskSnapshot{}, fmt.Errorf("%w: task %s", orchestration.ErrTaskNotReady, task.ID)
		}
	}
	if transition.ActorAgentID != "" {
		if err := agentBelongsToTeam(ctx, tx, transition.ActorAgentID, task.TeamID); err != nil {
			return orchestration.TaskSnapshot{}, err
		}
	}
	attempt := task.Attempt
	if transition.Target == orchestration.TaskReady && (task.State == orchestration.TaskClaimed || task.State == orchestration.TaskInProgress || task.State == orchestration.TaskVerification) {
		attempt++
	}
	now := time.Now().UTC()
	claimExpiresAt := ""
	if transition.Target == orchestration.TaskClaimed {
		claimExpiresAt = now.Add(defaultTaskClaimTTL).Format(time.RFC3339Nano)
	}
	if err := execCAS(ctx, tx, orchestration.ErrTaskStateConflict, `UPDATE tasks SET state = ?, assignee_agent_id = ?, claim_expires_at = ?, attempt = ?, version = version + 1, updated_at = ? WHERE task_id = ? AND version = ?`,
		string(transition.Target), string(assignee), claimExpiresAt, attempt, now.Format(time.RFC3339Nano), string(task.ID), task.Version); err != nil {
		return orchestration.TaskSnapshot{}, fmt.Errorf("update task state: %w", err)
	}
	streamSequence, err := currentOrchestrationSequence(ctx, tx, orchestration.StreamTask, string(task.ID))
	if err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	if _, err := appendOrchestrationEvents(ctx, tx, orchestration.Stream{Type: orchestration.StreamTask, ID: string(task.ID), TeamID: task.TeamID}, streamSequence,
		orchestration.PendingEvent{Type: orchestration.EventTaskUpdated, ActorAgentID: transition.ActorAgentID, CorrelationID: transition.CorrelationID, Payload: orchestration.MustJSONPayload(map[string]any{
			"from": task.State, "to": transition.Target, "assignee_agent_id": assignee, "attempt": attempt, "reason": transition.Reason,
		})},
	); err != nil {
		return orchestration.TaskSnapshot{}, fmt.Errorf("record task transition: %w", err)
	}
	if transition.Target == orchestration.TaskCompleted {
		if err := unblockDependentTasksTx(ctx, tx, task.ID, transition.ActorAgentID, transition.CorrelationID); err != nil {
			return orchestration.TaskSnapshot{}, fmt.Errorf("unblock dependent tasks: %w", err)
		}
	}
	updated, err := taskByID(ctx, tx, task.ID)
	if err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return orchestration.TaskSnapshot{}, fmt.Errorf("commit task transition: %w", err)
	}
	return updated, nil
}

func hashTaskUpdate(update orchestration.TaskUpdate) string {
	request := struct {
		TaskID              orchestration.TaskID  `json:"task_id"`
		ActorAgentID        orchestration.AgentID `json:"actor_agent_id"`
		Expected            uint64                `json:"expected"`
		Title               *string               `json:"title,omitempty"`
		Description         *string               `json:"description,omitempty"`
		Priority            *int                  `json:"priority,omitempty"`
		RequiredProfile     *string               `json:"required_profile,omitempty"`
		VerificationProfile *string               `json:"verification_profile,omitempty"`
	}{update.TaskID, update.ActorAgentID, update.Expected, update.Title, update.Description, update.Priority, update.RequiredProfile, update.VerificationProfile}
	encoded, _ := json.Marshal(request)
	return sha256Hex(encoded)
}

// UpdateTask changes plan metadata with optimistic concurrency and an
// idempotency record. Once execution has started, the plan is immutable so a
// resumed Agent cannot silently receive a different contract.
func (store *OrchestrationStore) UpdateTask(ctx context.Context, update orchestration.TaskUpdate) (orchestration.TaskSnapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := update.TaskID.Validate(); err != nil {
		return orchestration.TaskSnapshot{}, fmt.Errorf("invalid task update ID: %w", err)
	}
	if update.ActorAgentID != "" {
		if err := update.ActorAgentID.Validate(); err != nil {
			return orchestration.TaskSnapshot{}, fmt.Errorf("invalid task update actor: %w", err)
		}
	}
	if update.Title == nil && update.Description == nil && update.Priority == nil && update.RequiredProfile == nil && update.VerificationProfile == nil {
		return orchestration.TaskSnapshot{}, fmt.Errorf("%w: no task fields supplied", orchestration.ErrInvalidEvent)
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return orchestration.TaskSnapshot{}, fmt.Errorf("begin task update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	task, err := taskByID(ctx, tx, update.TaskID)
	if errors.Is(err, sql.ErrNoRows) {
		return orchestration.TaskSnapshot{}, fmt.Errorf("%w: task %s", orchestration.ErrNotFound, update.TaskID)
	}
	if err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	if update.ActorAgentID != "" {
		if err := agentBelongsToTeam(ctx, tx, update.ActorAgentID, task.TeamID); err != nil {
			return orchestration.TaskSnapshot{}, err
		}
	}
	requestHash := hashTaskUpdate(update)
	if update.IdempotencyKey != "" {
		var resultID, storedHash string
		lookupErr := tx.QueryRowContext(ctx, `SELECT result_id, request_hash FROM idempotency_records WHERE scope = ? AND idempotency_key = ?`, "task-update:"+string(update.TaskID), update.IdempotencyKey).Scan(&resultID, &storedHash)
		if lookupErr == nil {
			if storedHash != requestHash {
				return orchestration.TaskSnapshot{}, fmt.Errorf("%w: task update key %q", orchestration.ErrIdempotencyConflict, update.IdempotencyKey)
			}
			existing, getErr := taskByID(ctx, tx, update.TaskID)
			if getErr != nil {
				return orchestration.TaskSnapshot{}, getErr
			}
			if err := tx.Commit(); err != nil {
				return orchestration.TaskSnapshot{}, fmt.Errorf("commit idempotent task update lookup: %w", err)
			}
			return existing, nil
		}
		if !errors.Is(lookupErr, sql.ErrNoRows) {
			return orchestration.TaskSnapshot{}, fmt.Errorf("inspect task update idempotency key: %w", lookupErr)
		}
	}
	if err := requireActiveTeam(ctx, tx, task.TeamID); err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	if err := authorizeTaskPlanActorTx(ctx, tx, task, update.ActorAgentID); err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	if update.Expected == 0 || update.Expected == orchestration.AnySequence || update.Expected != task.Version {
		return orchestration.TaskSnapshot{}, fmt.Errorf("%w: expected %d, current %d", orchestration.ErrTaskStateConflict, update.Expected, task.Version)
	}
	switch task.State {
	case orchestration.TaskDraft, orchestration.TaskBlocked, orchestration.TaskReady:
	default:
		return orchestration.TaskSnapshot{}, fmt.Errorf("%w: task %s is %s", orchestration.ErrTaskEditNotAllowed, task.ID, task.State)
	}
	var live bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM agents WHERE assigned_task_id = ? AND state IN ('created','queued','running','waiting','canceling'))`, string(task.ID)).Scan(&live); err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	if live {
		return orchestration.TaskSnapshot{}, fmt.Errorf("%w: task %s has a live Agent", orchestration.ErrTaskEditNotAllowed, task.ID)
	}
	if open, err := openMerges(ctx, tx, task.ID); err != nil {
		return orchestration.TaskSnapshot{}, err
	} else if open > 0 {
		return orchestration.TaskSnapshot{}, fmt.Errorf("%w: task %s has unmerged work", orchestration.ErrTaskEditNotAllowed, task.ID)
	}
	title, description := task.Title, task.Description
	priority := task.Priority
	requiredProfile, verificationProfile := task.RequiredProfile, task.VerificationProfile
	if update.Title != nil {
		title = strings.TrimSpace(*update.Title)
		if title == "" || len(title) > 512 {
			return orchestration.TaskSnapshot{}, fmt.Errorf("%w: task title must be 1..512 bytes", orchestration.ErrInvalidEvent)
		}
	}
	if update.Description != nil {
		description = *update.Description
		if len(description) > 1024*1024 {
			return orchestration.TaskSnapshot{}, fmt.Errorf("%w: task description exceeds 1 MiB", orchestration.ErrInvalidEvent)
		}
	}
	if update.Priority != nil {
		priority = *update.Priority
	}
	if update.RequiredProfile != nil {
		requiredProfile = strings.TrimSpace(*update.RequiredProfile)
	}
	if update.VerificationProfile != nil {
		verificationProfile = strings.TrimSpace(*update.VerificationProfile)
	}
	now := time.Now().UTC()
	if err := execCAS(ctx, tx, orchestration.ErrTaskStateConflict, `UPDATE tasks SET title = ?, description = ?, priority = ?, required_profile = ?, verification_profile = ?, version = version + 1, updated_at = ? WHERE task_id = ? AND version = ?`, title, description, priority, requiredProfile, verificationProfile, now.Format(time.RFC3339Nano), string(task.ID), task.Version); err != nil {
		return orchestration.TaskSnapshot{}, fmt.Errorf("update task plan: %w", err)
	}
	if update.IdempotencyKey != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO idempotency_records(scope, idempotency_key, result_type, result_id, request_hash, created_at) VALUES(?, ?, ?, ?, ?, ?)`, "task-update:"+string(update.TaskID), update.IdempotencyKey, "task-update", string(task.ID), requestHash, now.Format(time.RFC3339Nano)); err != nil {
			return orchestration.TaskSnapshot{}, fmt.Errorf("record task update idempotency key: %w", err)
		}
	}
	sequence, err := currentOrchestrationSequence(ctx, tx, orchestration.StreamTask, string(task.ID))
	if err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	if _, err := appendOrchestrationEvents(ctx, tx, orchestration.Stream{Type: orchestration.StreamTask, ID: string(task.ID), TeamID: task.TeamID}, sequence, orchestration.PendingEvent{Type: orchestration.EventTaskUpdated, ActorAgentID: update.ActorAgentID, CorrelationID: update.CorrelationID, IdempotencyKey: update.IdempotencyKey, Payload: orchestration.MustJSONPayload(map[string]any{"from_version": task.Version, "to_version": task.Version + 1, "before": task, "title": title, "description": description, "priority": priority, "required_profile": requiredProfile, "verification_profile": verificationProfile, "reason": update.Reason})}); err != nil {
		return orchestration.TaskSnapshot{}, fmt.Errorf("record task plan update: %w", err)
	}
	updated, err := taskByID(ctx, tx, task.ID)
	if err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return orchestration.TaskSnapshot{}, fmt.Errorf("commit task plan update: %w", err)
	}
	return updated, nil
}

func validateTaskState(state orchestration.TaskState) error {
	switch state {
	case orchestration.TaskDraft, orchestration.TaskBlocked, orchestration.TaskReady, orchestration.TaskClaimed, orchestration.TaskInProgress, orchestration.TaskVerification, orchestration.TaskAwaitingMerge, orchestration.TaskCompleted, orchestration.TaskFailed, orchestration.TaskCanceled:
		return nil
	default:
		return fmt.Errorf("%w: unsupported task state %q", orchestration.ErrInvalidEvent, state)
	}
}

// openMerges counts the task's merges that have not been applied,
// rejected or superseded yet.
func openMerges(ctx context.Context, query rowQuery, taskID orchestration.TaskID) (int, error) {
	var count int
	err := query.QueryRowContext(ctx, `SELECT COUNT(*) FROM merge_queue WHERE task_id = ? AND state IN ('pending', 'running', 'conflicted')`, string(taskID)).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count open merges: %w", err)
	}
	return count, nil
}

func isTerminalTask(state orchestration.TaskState) bool {
	return state == orchestration.TaskCompleted || state == orchestration.TaskFailed || state == orchestration.TaskCanceled
}

// agentBelongsToTeam rejects an unknown Agent or an Agent of another Team as
// a task actor, task assignee or workspace view owner.
func agentBelongsToTeam(ctx context.Context, query rowQuery, agentID orchestration.AgentID, teamID orchestration.TeamID) error {
	var storedTeam string
	err := query.QueryRowContext(ctx, `SELECT team_id FROM agents WHERE agent_id = ?`, string(agentID)).Scan(&storedTeam)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: Agent %s", orchestration.ErrNotFound, agentID)
	}
	if err != nil {
		return fmt.Errorf("inspect Agent %s: %w", agentID, err)
	}
	if storedTeam != string(teamID) {
		return fmt.Errorf("%w: Agent %s belongs to another team", orchestration.ErrInvalidEvent, agentID)
	}
	return nil
}

var _ orchestration.TaskRepository = (*OrchestrationStore)(nil)

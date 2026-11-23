package atestore

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

var _ orchestration.TaskRepository = (*Store)(nil)
var _ orchestration.TaskClaimReaper = (*Store)(nil)

func (store *Store) CreateTask(ctx context.Context, spec orchestration.TaskSpec) (orchestration.TaskSnapshot, error) {
	if err := spec.Validate(); err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	request := spec
	request.CorrelationID, request.IdempotencyKey = "", ""
	hash := taskHash(request)
	var result orchestration.TaskSnapshot
	err := store.authority.Update(taskContext(ctx), func(tx *ateauthority.Tx) error {
		key := taskPair(string(spec.TeamID), spec.IdempotencyKey)
		if spec.IdempotencyKey != "" {
			var receipt taskReceipt
			found, err := tx.Get(recordKey("task-create-receipt", key), &receipt)
			if err != nil {
				return err
			}
			if found {
				if receipt.Hash != hash {
					return orchestration.ErrIdempotencyConflict
				}
				result, err = getRecord[orchestration.TaskSnapshot](tx, "task", string(receipt.TaskID))
				return err
			}
		}
		contractHash := ""
		if !spec.Contract.IsZero() {
			var hashErr error
			contractHash, hashErr = spec.Contract.Hash()
			if hashErr != nil {
				return hashErr
			}
		}
		// SQL's foreign key requires the team even when no actor is supplied.
		if _, err := getRecord[orchestration.TeamSnapshot](tx, "team", string(spec.TeamID)); err != nil {
			return err
		}
		if spec.ParentTaskID != "" {
			parent, err := getRecord[orchestration.TaskSnapshot](tx, "task", string(spec.ParentTaskID))
			if err != nil {
				return err
			}
			if parent.TeamID != spec.TeamID {
				return fmt.Errorf("%w: parent task belongs to another team", orchestration.ErrInvalidEvent)
			}
		}
		if spec.ActorAgentID != "" {
			if err := taskAgentBelongs(tx, spec.ActorAgentID, spec.TeamID); err != nil {
				return err
			}
		}
		id, err := orchestration.NewTaskID()
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		result = orchestration.TaskSnapshot{ID: id, TeamID: spec.TeamID, ParentTaskID: spec.ParentTaskID, Title: spec.Title, Description: spec.Description, Contract: spec.Contract, ContractHash: contractHash, State: orchestration.TaskDraft, Priority: spec.Priority, RequiredProfile: spec.RequiredProfile, VerificationProfile: spec.VerificationProfile, Version: 1, CreatedAt: now, UpdatedAt: now}
		if err := createRecord(tx, "task", string(id), result); err != nil {
			return err
		}
		if spec.IdempotencyKey != "" {
			if err := createRecord(tx, "task-create-receipt", key, taskReceipt{Hash: hash, TaskID: id}); err != nil {
				return err
			}
		}
		return taskEvent(tx, result, orchestration.PendingEvent{Type: orchestration.EventTaskCreated, ActorAgentID: spec.ActorAgentID, CorrelationID: spec.CorrelationID, IdempotencyKey: spec.IdempotencyKey, Payload: orchestration.MustJSONPayload(map[string]any{"parent_task_id": spec.ParentTaskID, "title": spec.Title, "description": spec.Description, "contract": spec.Contract, "priority": spec.Priority, "required_profile": spec.RequiredProfile, "verification_profile": spec.VerificationProfile})})
	})
	if err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	return result, nil
}

func (store *Store) GetTask(ctx context.Context, id orchestration.TaskID) (orchestration.TaskSnapshot, error) {
	if err := id.Validate(); err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	var result orchestration.TaskSnapshot
	err := store.authority.View(taskContext(ctx), func(tx *ateauthority.Tx) error {
		var err error
		result, err = getRecord[orchestration.TaskSnapshot](tx, "task", string(id))
		return err
	})
	if err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	return result, nil
}

func (store *Store) ListTasks(ctx context.Context, query orchestration.TaskQuery) ([]orchestration.TaskSnapshot, error) {
	if err := query.TeamID.Validate(); err != nil {
		return nil, err
	}
	if query.ParentTaskID != "" {
		if err := query.ParentTaskID.Validate(); err != nil {
			return nil, err
		}
	}
	if query.AssigneeAgentID != "" {
		if err := query.AssigneeAgentID.Validate(); err != nil {
			return nil, err
		}
	}
	for _, state := range query.States {
		if err := taskValidateState(state); err != nil {
			return nil, err
		}
	}
	result := make([]orchestration.TaskSnapshot, 0)
	err := store.authority.View(taskContext(ctx), func(tx *ateauthority.Tx) error {
		tasks, err := listRecords[orchestration.TaskSnapshot](tx, "task")
		if err != nil {
			return err
		}
		for _, task := range tasks {
			if task.TeamID != query.TeamID || query.ParentTaskID != "" && task.ParentTaskID != query.ParentTaskID || query.AssigneeAgentID != "" && task.AssigneeAgentID != query.AssigneeAgentID || len(query.States) > 0 && !slices.Contains(query.States, task.State) {
				continue
			}
			result = append(result, task)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	taskSort(result)
	return result, nil
}

func (store *Store) TransitionTask(ctx context.Context, transition orchestration.TaskTransition) (orchestration.TaskSnapshot, error) {
	if err := taskValidateTransition(transition); err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	var result orchestration.TaskSnapshot
	err := store.authority.Update(taskContext(ctx), func(tx *ateauthority.Tx) error {
		var err error
		result, err = transitionTaskTx(tx, transition, time.Now().UTC())
		return err
	})
	if err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	return result, nil
}

func taskValidateTransition(transition orchestration.TaskTransition) error {
	if err := transition.TaskID.Validate(); err != nil {
		return err
	}
	if err := taskValidateState(transition.Target); err != nil {
		return err
	}
	if transition.AssigneeAgentID != "" {
		if err := transition.AssigneeAgentID.Validate(); err != nil {
			return err
		}
	}
	if transition.ActorAgentID != "" {
		if err := transition.ActorAgentID.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// transitionTaskTx performs no nested authority/session calls. Its caller owns
// the atomic transaction, including any agent or workspace mutations.
func transitionTaskTx(tx *ateauthority.Tx, transition orchestration.TaskTransition, now time.Time) (orchestration.TaskSnapshot, error) {
	var zero orchestration.TaskSnapshot
	if err := taskValidateTransition(transition); err != nil {
		return zero, err
	}
	task, err := getRecord[orchestration.TaskSnapshot](tx, "task", string(transition.TaskID))
	if err != nil {
		return zero, err
	}
	if transition.Expected != orchestration.AnySequence && transition.Expected != task.Version {
		return zero, fmt.Errorf("%w: expected %d, current %d", orchestration.ErrTaskStateConflict, transition.Expected, task.Version)
	}
	target := transition.Target
	if target == orchestration.TaskVerification || target == orchestration.TaskAwaitingMerge || target == orchestration.TaskCompleted {
		complete, err := taskDependenciesComplete(tx, task.ID, "task-refinement")
		if err != nil {
			return zero, err
		}
		if !complete {
			return zero, fmt.Errorf("%w: refinement children are not completed", orchestration.ErrTaskNotReady)
		}
	}
	if (target == orchestration.TaskCompleted || target == orchestration.TaskAwaitingMerge) && task.VerificationProfile != "" {
		if task.State == orchestration.TaskInProgress {
			return zero, orchestration.ErrTaskVerificationRequired
		}
		if (target == orchestration.TaskAwaitingMerge || task.State == orchestration.TaskVerification) && (transition.ActorAgentID == "" || transition.ActorAgentID == task.AssigneeAgentID) {
			return zero, orchestration.ErrTaskReviewerRequired
		}
	}
	if target == orchestration.TaskAwaitingMerge || target == orchestration.TaskCompleted {
		open, err := taskOpenMerges(tx, task.ID)
		if err != nil {
			return zero, err
		}
		if target == orchestration.TaskAwaitingMerge && open == 0 {
			return zero, fmt.Errorf("%w: task has no queued merge", orchestration.ErrInvalidEvent)
		}
		if target == orchestration.TaskCompleted && open > 0 {
			return zero, fmt.Errorf("%w: task has unmerged requests", orchestration.ErrInvalidEvent)
		}
	}
	if (target == orchestration.TaskClaimed || target == orchestration.TaskInProgress) && task.State == orchestration.TaskBlocked {
		return zero, orchestration.ErrTaskNotReady
	}
	if target == orchestration.TaskClaimed && task.AssigneeAgentID != "" && task.AssigneeAgentID != transition.AssigneeAgentID {
		return zero, orchestration.ErrTaskClaimConflict
	}
	if err := orchestration.ValidateTaskTransition(task.State, target); err != nil {
		return zero, err
	}
	assignee := task.AssigneeAgentID
	if transition.AssigneeAgentID != "" {
		assignee = transition.AssigneeAgentID
	}
	if target == orchestration.TaskReady {
		if task.State != orchestration.TaskVerification && transition.ActorAgentID != "" && task.AssigneeAgentID != "" && task.AssigneeAgentID != transition.ActorAgentID {
			return zero, fmt.Errorf("%w: task is assigned to %s", orchestration.ErrInvalidEvent, task.AssigneeAgentID)
		}
		if task.State != orchestration.TaskClaimed && task.State != orchestration.TaskInProgress && task.State != orchestration.TaskVerification {
			complete, err := taskDependenciesComplete(tx, task.ID, "task-dependency")
			if err != nil {
				return zero, err
			}
			if !complete {
				return zero, orchestration.ErrTaskNotReady
			}
		}
		assignee = ""
	}
	if target == orchestration.TaskBlocked {
		assignee = ""
	}
	if target == orchestration.TaskClaimed || target == orchestration.TaskInProgress {
		if assignee == "" {
			return zero, fmt.Errorf("%w: task requires an assignee", orchestration.ErrInvalidEvent)
		}
		if err := taskAgentBelongs(tx, assignee, task.TeamID); err != nil {
			return zero, err
		}
		complete, err := taskDependenciesComplete(tx, task.ID, "task-dependency")
		if err != nil {
			return zero, err
		}
		if !complete {
			return zero, orchestration.ErrTaskNotReady
		}
	}
	if transition.ActorAgentID != "" {
		if err := taskAgentBelongs(tx, transition.ActorAgentID, task.TeamID); err != nil {
			return zero, err
		}
	}
	before := task.State
	if target == orchestration.TaskReady && (before == orchestration.TaskClaimed || before == orchestration.TaskInProgress || before == orchestration.TaskVerification) {
		task.Attempt++
	}
	task.State, task.AssigneeAgentID, task.ClaimExpiresAt = target, assignee, nil
	if target == orchestration.TaskClaimed {
		expiry := now.UTC().Add(taskClaimTTL)
		task.ClaimExpiresAt = &expiry
	}
	task.Version++
	task.UpdatedAt = now.UTC()
	if err := putRecord(tx, "task", string(task.ID), task); err != nil {
		return zero, err
	}
	if err := taskEvent(tx, task, orchestration.PendingEvent{Type: orchestration.EventTaskUpdated, ActorAgentID: transition.ActorAgentID, CorrelationID: transition.CorrelationID, Payload: orchestration.MustJSONPayload(map[string]any{"from": before, "to": target, "assignee_agent_id": assignee, "attempt": task.Attempt, "reason": transition.Reason})}); err != nil {
		return zero, err
	}
	if target == orchestration.TaskCompleted {
		if err := taskUnblockDependents(tx, task.ID, transition.ActorAgentID, transition.CorrelationID, now); err != nil {
			return zero, err
		}
	}
	return task, nil
}

func claimTaskTx(tx *ateauthority.Tx, taskID orchestration.TaskID, agentID orchestration.AgentID, expected uint64, now time.Time) (orchestration.TaskSnapshot, error) {
	task, err := getRecord[orchestration.TaskSnapshot](tx, "task", string(taskID))
	if err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	if expected != orchestration.AnySequence && expected != task.Version {
		return orchestration.TaskSnapshot{}, orchestration.ErrTaskStateConflict
	}
	if task.State != orchestration.TaskReady || task.AssigneeAgentID != "" {
		return orchestration.TaskSnapshot{}, orchestration.ErrTaskClaimConflict
	}
	return transitionTaskTx(tx, orchestration.TaskTransition{TaskID: taskID, Expected: expected, Target: orchestration.TaskClaimed, AssigneeAgentID: agentID, ActorAgentID: agentID, Reason: "claimed by spawned Agent"}, now)
}

func (store *Store) ClaimTask(ctx context.Context, id orchestration.TaskID, agent orchestration.AgentID, expected uint64) (orchestration.TaskSnapshot, error) {
	return store.TransitionTask(ctx, orchestration.TaskTransition{TaskID: id, Expected: expected, Target: orchestration.TaskClaimed, AssigneeAgentID: agent, ActorAgentID: agent, Reason: "claimed"})
}

func (store *Store) ReleaseTask(ctx context.Context, id orchestration.TaskID, agent orchestration.AgentID, expected uint64) (orchestration.TaskSnapshot, error) {
	return store.TransitionTask(ctx, orchestration.TaskTransition{TaskID: id, Expected: expected, Target: orchestration.TaskReady, ActorAgentID: agent, Reason: "released"})
}

func (store *Store) ReapExpiredTaskClaims(ctx context.Context, now time.Time) (int, error) {
	return store.reapTaskClaims(ctx, "", now)
}

// ReapTeamTaskClaims repairs only the Team owned by the invoking runtime.
func (store *Store) ReapTeamTaskClaims(ctx context.Context, team orchestration.TeamID, now time.Time) (int, error) {
	if err := team.Validate(); err != nil {
		return 0, err
	}
	return store.reapTaskClaims(ctx, team, now)
}

// reapTaskClaims shares claim validation while keeping the sweep scope explicit.
func (store *Store) reapTaskClaims(ctx context.Context, team orchestration.TeamID, now time.Time) (int, error) {
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	count := 0
	err := store.authority.Update(taskContext(ctx), func(tx *ateauthority.Tx) error {
		count = 0 // Update may retry after a conclusively rejected root CAS.
		tasks, err := listRecords[orchestration.TaskSnapshot](tx, "task")
		if err != nil {
			return err
		}
		agents, err := listRecords[orchestration.AgentSnapshot](tx, "agent")
		if err != nil {
			return err
		}
		live := map[orchestration.TaskID]bool{}
		for _, agent := range agents {
			if taskAgentLive(agent.State) {
				live[agent.AssignedTaskID] = true
			}
		}
		slices.SortFunc(tasks, func(a, b orchestration.TaskSnapshot) int {
			if a.ID < b.ID {
				return -1
			}
			if a.ID > b.ID {
				return 1
			}
			return 0
		})
		for _, task := range tasks {
			if team != "" && task.TeamID != team {
				continue
			}
			if task.State != orchestration.TaskClaimed || task.ClaimExpiresAt == nil || task.ClaimExpiresAt.After(now) || live[task.ID] {
				continue
			}
			owner := task.AssigneeAgentID
			task.State, task.AssigneeAgentID, task.ClaimExpiresAt = orchestration.TaskReady, "", nil
			task.Attempt++
			task.Version++
			task.UpdatedAt = now
			if err := putRecord(tx, "task", string(task.ID), task); err != nil {
				return err
			}
			if err := taskEvent(tx, task, orchestration.PendingEvent{Type: orchestration.EventTaskClaimExpired, ActorAgentID: owner, Payload: orchestration.MustJSONPayload(map[string]any{"from": orchestration.TaskClaimed, "to": orchestration.TaskReady, "assignee_agent_id": owner, "attempt": "incremented", "reason": orchestration.ErrTaskClaimExpired.Error()})}); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

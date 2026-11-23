package atestore

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

var _ orchestration.TaskRefinementRepository = (*Store)(nil)

func taskRefinementHash(spec orchestration.TaskRefinementSpec) string {
	spec.CorrelationID, spec.IdempotencyKey = "", ""
	spec.Children = slices.Clone(spec.Children)
	spec.Dependencies = slices.Clone(spec.Dependencies)
	slices.SortFunc(spec.Children, func(a, b orchestration.TaskRefinementChild) int { return strings.Compare(a.Key, b.Key) })
	slices.SortFunc(spec.Dependencies, func(a, b orchestration.TaskRefinementDependency) int {
		left, _ := json.Marshal(a)
		right, _ := json.Marshal(b)
		return strings.Compare(string(left), string(right))
	})
	return taskHash(spec)
}

func (store *Store) RefineTasks(ctx context.Context, spec orchestration.TaskRefinementSpec) (orchestration.TaskRefinementResult, error) {
	if err := spec.Validate(); err != nil {
		return orchestration.TaskRefinementResult{}, err
	}
	hash := taskRefinementHash(spec)
	var result orchestration.TaskRefinementResult
	err := store.authority.Update(taskContext(ctx), func(tx *ateauthority.Tx) error {
		key := taskPair(string(spec.TeamID), spec.IdempotencyKey)
		var receipt taskReceipt
		found, err := tx.Get(recordKey("task-refinement-receipt", key), &receipt)
		if err != nil {
			return err
		}
		if found {
			if receipt.Hash != hash {
				return orchestration.ErrIdempotencyConflict
			}
			if len(receipt.Children) != len(spec.Children) {
				return fmt.Errorf("%w: invalid refinement receipt", orchestration.ErrInvalidEvent)
			}
			for _, child := range spec.Children {
				if receipt.Children[child.Key] == "" {
					return fmt.Errorf("%w: missing refinement child", orchestration.ErrInvalidEvent)
				}
			}
			result, err = taskLoadRefinement(tx, spec.TeamID, spec.ParentTaskID, receipt.Children)
			return err
		}
		if err := taskActiveTeam(tx, spec.TeamID); err != nil {
			return err
		}
		parent, err := getRecord[orchestration.TaskSnapshot](tx, "task", string(spec.ParentTaskID))
		if err != nil {
			return err
		}
		if parent.TeamID != spec.TeamID {
			return fmt.Errorf("%w: parent belongs to another team", orchestration.ErrInvalidEvent)
		}
		if parent.Version != spec.Expected {
			return orchestration.ErrTaskStateConflict
		}
		switch parent.State {
		case orchestration.TaskDraft, orchestration.TaskBlocked, orchestration.TaskReady, orchestration.TaskClaimed, orchestration.TaskInProgress:
		default:
			return fmt.Errorf("%w: parent is %s", orchestration.ErrTaskRefinementInvalid, parent.State)
		}
		if err := taskAuthorizePlan(tx, parent, spec.ActorAgentID); err != nil {
			return err
		}
		ids := make(map[string]orchestration.TaskID, len(spec.Children))
		now := time.Now().UTC()
		for _, child := range spec.Children {
			id, err := orchestration.NewTaskID()
			if err != nil {
				return err
			}
			ids[child.Key] = id
			contractHash := ""
			if !child.Contract.IsZero() {
				contractHash, err = child.Contract.Hash()
				if err != nil {
					return err
				}
			}
			task := orchestration.TaskSnapshot{ID: id, TeamID: spec.TeamID, ParentTaskID: parent.ID, Title: child.Title, Description: child.Description, Contract: child.Contract, ContractHash: contractHash, Priority: child.Priority, RequiredProfile: child.RequiredProfile, VerificationProfile: child.VerificationProfile, State: orchestration.TaskDraft, Version: 1, CreatedAt: now, UpdatedAt: now}
			if err := createRecord(tx, "task", string(id), task); err != nil {
				return err
			}
			if err := createRecord(tx, "task-refinement", taskPair(string(parent.ID), string(id)), taskEdge{TaskID: parent.ID, DependencyID: id, CreatedAt: now}); err != nil {
				return err
			}
			if err := taskEvent(tx, task, orchestration.PendingEvent{Type: orchestration.EventTaskCreated, ActorAgentID: spec.ActorAgentID, CorrelationID: spec.CorrelationID, IdempotencyKey: spec.IdempotencyKey, Payload: orchestration.MustJSONPayload(map[string]any{"parent_task_id": parent.ID, "title": child.Title, "description": child.Description, "contract": child.Contract, "contract_hash": contractHash, "priority": child.Priority, "required_profile": child.RequiredProfile, "verification_profile": child.VerificationProfile, "refinement_key": child.Key})}); err != nil {
				return err
			}
		}
		for _, edge := range spec.Dependencies {
			id, dependencyID := ids[edge.ChildKey], edge.DependencyTaskID
			if edge.DependencyChildKey != "" {
				dependencyID = ids[edge.DependencyChildKey]
			}
			dependency, err := getRecord[orchestration.TaskSnapshot](tx, "task", string(dependencyID))
			if err != nil {
				return err
			}
			if dependency.TeamID != spec.TeamID {
				return fmt.Errorf("%w: dependency belongs to another team", orchestration.ErrInvalidEvent)
			}
			if err := taskCheckCycle(tx, id, dependencyID); err != nil {
				return err
			}
			if err := createRecord(tx, "task-dependency", taskPair(string(id), string(dependencyID)), taskEdge{TaskID: id, DependencyID: dependencyID, CreatedAt: now}); err != nil {
				return err
			}
			// Like SQL refinement, children remain draft until explicitly scheduled.
			if err := taskEvent(tx, orchestration.TaskSnapshot{ID: id, TeamID: spec.TeamID}, orchestration.PendingEvent{Type: orchestration.EventTaskDependencyAdded, ActorAgentID: spec.ActorAgentID, CorrelationID: spec.CorrelationID, Payload: orchestration.MustJSONPayload(map[string]any{"task_id": id, "dependency_task_id": dependencyID, "refinement": true})}); err != nil {
				return err
			}
		}
		parent.Version++
		parent.UpdatedAt = now
		if err := putRecord(tx, "task", string(parent.ID), parent); err != nil {
			return err
		}
		if err := taskEvent(tx, parent, orchestration.PendingEvent{Type: orchestration.EventTaskUpdated, ActorAgentID: spec.ActorAgentID, CorrelationID: spec.CorrelationID, IdempotencyKey: spec.IdempotencyKey, Payload: orchestration.MustJSONPayload(map[string]any{"reason": "task refined", "children": ids, "version": parent.Version})}); err != nil {
			return err
		}
		if err := createRecord(tx, "task-refinement-receipt", key, taskReceipt{Hash: hash, TaskID: parent.ID, Children: ids}); err != nil {
			return err
		}
		result, err = taskLoadRefinement(tx, spec.TeamID, parent.ID, ids)
		return err
	})
	if err != nil {
		return orchestration.TaskRefinementResult{}, err
	}
	return result, nil
}

func taskLoadRefinement(tx *ateauthority.Tx, team orchestration.TeamID, parent orchestration.TaskID, ids map[string]orchestration.TaskID) (orchestration.TaskRefinementResult, error) {
	result := orchestration.TaskRefinementResult{ParentTaskID: parent, Children: make(map[string]orchestration.TaskSnapshot, len(ids))}
	for key, id := range ids {
		task, err := getRecord[orchestration.TaskSnapshot](tx, "task", string(id))
		if err != nil {
			return orchestration.TaskRefinementResult{}, err
		}
		if task.TeamID != team || task.ParentTaskID != parent {
			return orchestration.TaskRefinementResult{}, fmt.Errorf("%w: refinement child differs from parent", orchestration.ErrInvalidEvent)
		}
		var edge taskEdge
		found, err := tx.Get(recordKey("task-refinement", taskPair(string(parent), string(id))), &edge)
		if err != nil {
			return orchestration.TaskRefinementResult{}, err
		}
		if !found || edge.TaskID != parent || edge.DependencyID != id {
			return orchestration.TaskRefinementResult{}, fmt.Errorf("%w: missing refinement membership", orchestration.ErrInvalidEvent)
		}
		result.Children[key] = task
	}
	return result, nil
}

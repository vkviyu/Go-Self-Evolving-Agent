package atestore

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

// AddDependency adds a prerequisite edge subject to the repository's task and cycle
// checks.
func (store *Store) AddDependency(ctx context.Context, id, dependency orchestration.TaskID, actor orchestration.AgentID, correlation string) error {
	return store.taskChangeDependency(ctx, id, dependency, actor, correlation, true)
}

// RemoveDependency deletes the prerequisite edge and records the change in the task's
// history.
func (store *Store) RemoveDependency(ctx context.Context, id, dependency orchestration.TaskID, actor orchestration.AgentID, correlation string) error {
	return store.taskChangeDependency(ctx, id, dependency, actor, correlation, false)
}

// ListDependencies returns the prerequisite IDs recorded for this task.
func (store *Store) ListDependencies(ctx context.Context, id orchestration.TaskID) ([]orchestration.TaskID, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	result := make([]orchestration.TaskID, 0)
	err := store.authority.View(taskContext(ctx), func(tx *ateauthority.Tx) error {
		edges, err := listRecords[taskEdge](tx, "task-dependency")
		if err != nil {
			return err
		}
		for _, edge := range edges {
			if edge.TaskID == id {
				result = append(result, edge.DependencyID)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(result)
	return result, nil
}

func (store *Store) taskChangeDependency(ctx context.Context, id, dependencyID orchestration.TaskID, actor orchestration.AgentID, correlation string, add bool) error {
	if err := id.Validate(); err != nil {
		return err
	}
	if err := dependencyID.Validate(); err != nil {
		return err
	}
	if id == dependencyID {
		return orchestration.ErrTaskDependencyCycle
	}
	if actor != "" {
		if err := actor.Validate(); err != nil {
			return err
		}
	}
	return store.authority.Update(taskContext(ctx), func(tx *ateauthority.Tx) error {
		task, err := getRecord[orchestration.TaskSnapshot](tx, "task", string(id))
		if err != nil {
			return err
		}
		dependency, err := getRecord[orchestration.TaskSnapshot](tx, "task", string(dependencyID))
		if err != nil {
			return err
		}
		if task.TeamID != dependency.TeamID {
			return fmt.Errorf("%w: dependency belongs to another team", orchestration.ErrInvalidEvent)
		}
		if taskTerminal(task.State) {
			return fmt.Errorf("%w: terminal task cannot change dependencies", orchestration.ErrInvalidEvent)
		}
		if actor != "" {
			if err := taskAgentBelongs(tx, actor, task.TeamID); err != nil {
				return err
			}
		}
		if add {
			if err := taskCheckCycle(tx, id, dependencyID); err != nil {
				return err
			}
		}
		key := taskPair(string(id), string(dependencyID))
		var existing taskEdge
		found, err := tx.Get(recordKey("task-dependency", key), &existing)
		if err != nil {
			return err
		}
		if found == add {
			return nil
		}
		now := time.Now().UTC()
		if add {
			if err := createRecord(tx, "task-dependency", key, taskEdge{TaskID: id, DependencyID: dependencyID, CreatedAt: now}); err != nil {
				return err
			}
		} else if err := tx.Delete(recordKey("task-dependency", key)); err != nil {
			return err
		}
		before := task.State
		if add && (before == orchestration.TaskDraft || before == orchestration.TaskReady) || !add && before == orchestration.TaskBlocked {
			complete, err := taskDependenciesComplete(tx, id, "task-dependency")
			if err != nil {
				return err
			}
			if add && !complete {
				task.State = orchestration.TaskBlocked
			}
			if !add && complete {
				task.State = orchestration.TaskReady
			}
		}
		kind := orchestration.EventTaskDependencyAdded
		if !add {
			kind = orchestration.EventTaskDependencyRemoved
		}
		pending := []orchestration.PendingEvent{{Type: kind, ActorAgentID: actor, CorrelationID: correlation, Payload: orchestration.MustJSONPayload(map[string]any{"task_id": id, "dependency_task_id": dependencyID})}}
		if task.State != before {
			task.AssigneeAgentID = ""
			task.Version++
			task.UpdatedAt = now
			if err := putRecord(tx, "task", string(id), task); err != nil {
				return err
			}
			pending = append(pending, orchestration.PendingEvent{Type: orchestration.EventTaskUpdated, ActorAgentID: actor, CorrelationID: correlation, Payload: orchestration.MustJSONPayload(map[string]any{"from": before, "to": task.State, "assignee_agent_id": "", "reason": "dependency readiness"})})
		}
		return taskEvent(tx, task, pending...)
	})
}

func taskUnblockDependents(tx *ateauthority.Tx, dependency orchestration.TaskID, actor orchestration.AgentID, correlation string, now time.Time) error {
	edges, err := listRecords[taskEdge](tx, "task-dependency")
	if err != nil {
		return err
	}
	ids := make([]orchestration.TaskID, 0)
	for _, edge := range edges {
		if edge.DependencyID == dependency {
			ids = append(ids, edge.TaskID)
		}
	}
	slices.Sort(ids)
	for _, id := range ids {
		task, err := getRecord[orchestration.TaskSnapshot](tx, "task", string(id))
		if err != nil {
			return err
		}
		if task.State != orchestration.TaskBlocked {
			continue
		}
		complete, err := taskDependenciesComplete(tx, id, "task-dependency")
		if err != nil {
			return err
		}
		if !complete {
			continue
		}
		task.State = orchestration.TaskReady
		task.Version++
		task.UpdatedAt = now.UTC()
		if err := putRecord(tx, "task", string(id), task); err != nil {
			return err
		}
		if err := taskEvent(tx, task, orchestration.PendingEvent{Type: orchestration.EventTaskUpdated, ActorAgentID: actor, CorrelationID: correlation, Payload: orchestration.MustJSONPayload(map[string]any{"from": orchestration.TaskBlocked, "to": orchestration.TaskReady, "assignee_agent_id": "", "reason": "dependencies completed", "dependency_task_id": dependency})}); err != nil {
			return err
		}
	}
	return nil
}

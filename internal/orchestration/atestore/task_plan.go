package atestore

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

var _ orchestration.TaskPlanRepository = (*Store)(nil)

func (store *Store) UpdateTask(ctx context.Context, update orchestration.TaskUpdate) (orchestration.TaskSnapshot, error) {
	if err := update.TaskID.Validate(); err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	if update.ActorAgentID != "" {
		if err := update.ActorAgentID.Validate(); err != nil {
			return orchestration.TaskSnapshot{}, err
		}
	}
	if update.Title == nil && update.Description == nil && update.Priority == nil && update.RequiredProfile == nil && update.VerificationProfile == nil {
		return orchestration.TaskSnapshot{}, fmt.Errorf("%w: no task fields supplied", orchestration.ErrInvalidEvent)
	}
	request := update
	request.IdempotencyKey, request.CorrelationID, request.Reason = "", "", ""
	hash := taskHash(request)
	var result orchestration.TaskSnapshot
	err := store.authority.Update(taskContext(ctx), func(tx *ateauthority.Tx) error {
		task, err := getRecord[orchestration.TaskSnapshot](tx, "task", string(update.TaskID))
		if err != nil {
			return err
		}
		if update.ActorAgentID != "" {
			if err := taskAgentBelongs(tx, update.ActorAgentID, task.TeamID); err != nil {
				return err
			}
		}
		key := taskPair(string(task.ID), update.IdempotencyKey)
		if update.IdempotencyKey != "" {
			var receipt taskReceipt
			found, err := tx.Get(recordKey("task-update-receipt", key), &receipt)
			if err != nil {
				return err
			}
			if found {
				if receipt.Hash != hash {
					return orchestration.ErrIdempotencyConflict
				}
				result = task
				return nil
			}
		}
		if err := taskActiveTeam(tx, task.TeamID); err != nil {
			return err
		}
		if err := taskAuthorizePlan(tx, task, update.ActorAgentID); err != nil {
			return err
		}
		if update.Expected == 0 || update.Expected == orchestration.AnySequence || update.Expected != task.Version {
			return orchestration.ErrTaskStateConflict
		}
		switch task.State {
		case orchestration.TaskDraft, orchestration.TaskBlocked, orchestration.TaskReady:
		default:
			return orchestration.ErrTaskEditNotAllowed
		}
		agents, err := listRecords[orchestration.AgentSnapshot](tx, "agent")
		if err != nil {
			return err
		}
		for _, agent := range agents {
			if agent.AssignedTaskID == task.ID && taskAgentLive(agent.State) {
				return fmt.Errorf("%w: task has a live Agent", orchestration.ErrTaskEditNotAllowed)
			}
		}
		open, err := taskOpenMerges(tx, task.ID)
		if err != nil {
			return err
		}
		if open > 0 {
			return fmt.Errorf("%w: task has unmerged work", orchestration.ErrTaskEditNotAllowed)
		}
		before := task
		if update.Title != nil {
			task.Title = strings.TrimSpace(*update.Title)
			if task.Title == "" || len(task.Title) > 512 {
				return fmt.Errorf("%w: task title must be 1..512 bytes", orchestration.ErrInvalidEvent)
			}
		}
		if update.Description != nil {
			task.Description = *update.Description
			if len(task.Description) > 1024*1024 {
				return fmt.Errorf("%w: task description exceeds 1 MiB", orchestration.ErrInvalidEvent)
			}
		}
		if update.Priority != nil {
			task.Priority = *update.Priority
		}
		if update.RequiredProfile != nil {
			task.RequiredProfile = strings.TrimSpace(*update.RequiredProfile)
		}
		if update.VerificationProfile != nil {
			task.VerificationProfile = strings.TrimSpace(*update.VerificationProfile)
		}
		task.Version++
		task.UpdatedAt = time.Now().UTC()
		if err := putRecord(tx, "task", string(task.ID), task); err != nil {
			return err
		}
		if update.IdempotencyKey != "" {
			if err := createRecord(tx, "task-update-receipt", key, taskReceipt{Hash: hash, TaskID: task.ID}); err != nil {
				return err
			}
		}
		if err := taskEvent(tx, task, orchestration.PendingEvent{Type: orchestration.EventTaskUpdated, ActorAgentID: update.ActorAgentID, CorrelationID: update.CorrelationID, IdempotencyKey: update.IdempotencyKey, Payload: orchestration.MustJSONPayload(map[string]any{"from_version": before.Version, "to_version": task.Version, "before": before, "title": task.Title, "description": task.Description, "priority": task.Priority, "required_profile": task.RequiredProfile, "verification_profile": task.VerificationProfile, "reason": update.Reason})}); err != nil {
			return err
		}
		result = task
		return nil
	})
	if err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	return result, nil
}

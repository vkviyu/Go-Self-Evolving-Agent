package atestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

const mergeRecordKind = "merge"

func mergeSpecHash(spec workspacecoord.MergeSpec) string {
	base := struct {
		TeamID        orchestration.TeamID          `json:"team_id"`
		AgentID       orchestration.AgentID         `json:"agent_id"`
		TaskID        orchestration.TaskID          `json:"task_id"`
		ExecutionID   string                        `json:"execution_id,omitempty"`
		ContractHash  string                        `json:"contract_hash,omitempty"`
		Attempt       int                           `json:"attempt,omitempty"`
		ViewID        orchestration.WorkspaceViewID `json:"view_id"`
		BaseRevision  string                        `json:"base_revision"`
		PatchArtifact string                        `json:"patch_artifact"`
	}{spec.TeamID, spec.AgentID, spec.TaskID, spec.ExecutionID, spec.ContractHash, spec.Attempt, spec.ViewID, spec.BaseRevision, spec.PatchArtifact}
	if spec.Kind != workspacecoord.MergeSnapshot && spec.Supersedes == "" {
		return workspaceDigest(base)
	}
	encoded, _ := json.Marshal(base)
	return workspaceDigest(struct {
		Base       json.RawMessage          `json:"base"`
		Kind       workspacecoord.MergeKind `json:"kind"`
		Candidate  string                   `json:"candidate_digest"`
		Supersedes orchestration.MergeID    `json:"supersedes"`
	}{encoded, spec.Kind, spec.CandidateDigest, spec.Supersedes})
}

func mergeSpecMatches(merge workspacecoord.MergeRequest, spec workspacecoord.MergeSpec) bool {
	kind := spec.Kind
	if kind == "" {
		kind = workspacecoord.MergeGitPatch
	}
	return merge.TeamID == spec.TeamID && merge.AgentID == spec.AgentID && merge.TaskID == spec.TaskID &&
		merge.ExecutionID == spec.ExecutionID && merge.ContractHash == spec.ContractHash && merge.Attempt == spec.Attempt &&
		merge.ViewID == spec.ViewID && merge.BaseRevision == spec.BaseRevision &&
		merge.PatchArtifact == spec.PatchArtifact && merge.Kind == kind &&
		merge.CandidateDigest == spec.CandidateDigest && merge.Supersedes == spec.Supersedes
}

func (store *Store) EnqueueMerge(ctx context.Context, spec workspacecoord.MergeSpec) (workspacecoord.MergeRequest, error) {
	if err := spec.Validate(); err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	id, err := orchestration.NewMergeID()
	if err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	now, hash := time.Now().UTC(), mergeSpecHash(spec)
	var result workspacecoord.MergeRequest
	err = store.authority.Update(workspaceContext(ctx), func(tx *ateauthority.Tx) error {
		if spec.IdempotencyKey != "" {
			identity, err := getRecord[workspaceIdentity](tx, "merge-key", workspaceIdentityID(spec.TeamID, spec.IdempotencyKey))
			if err == nil {
				merge, err := getRecord[workspacecoord.MergeRequest](tx, mergeRecordKind, identity.ID)
				if err != nil {
					return err
				}
				legacy := spec
				legacy.Supersedes = ""
				legacyHash := identity.Hash == "" || spec.Kind != workspacecoord.MergeSnapshot && identity.Hash == mergeSpecHash(legacy)
				if !legacyHash && identity.Hash != hash || legacyHash && !mergeSpecMatches(merge, spec) {
					return fmt.Errorf("%w: merge key %q", orchestration.ErrIdempotencyConflict, spec.IdempotencyKey)
				}
				result = merge
				return nil
			}
			if !errors.Is(err, orchestration.ErrNotFound) {
				return err
			}
		}
		view, err := getRecord[workspacecoord.View](tx, workspaceViewKind, string(spec.ViewID))
		if err != nil {
			return err
		}
		if view.TeamID != spec.TeamID || view.AgentID != spec.AgentID {
			return fmt.Errorf("%w: merge view owner mismatch", orchestration.ErrInvalidEvent)
		}
		if view.State != workspacecoord.ViewActive {
			return fmt.Errorf("%w: merge view is %s", orchestration.ErrInvalidEvent, view.State)
		}
		if view.BaseRevision != "" && view.BaseRevision != spec.BaseRevision {
			return fmt.Errorf("%w: merge base revision does not match workspace view", orchestration.ErrInvalidEvent)
		}
		if view.TaskID != "" && view.TaskID != spec.TaskID {
			return fmt.Errorf("%w: merge task does not match workspace view", orchestration.ErrInvalidEvent)
		}
		if err := workspaceTaskTeam(tx, spec.TaskID, spec.TeamID); err != nil {
			return err
		}
		kind := spec.Kind
		if kind == "" {
			kind = workspacecoord.MergeGitPatch
		}
		merge := workspacecoord.MergeRequest{
			ID: id, TeamID: spec.TeamID, AgentID: spec.AgentID, TaskID: spec.TaskID,
			ExecutionID: spec.ExecutionID, ContractHash: spec.ContractHash, Attempt: spec.Attempt,
			ViewID: spec.ViewID, BaseRevision: spec.BaseRevision, PatchArtifact: spec.PatchArtifact,
			Kind: kind, CandidateDigest: spec.CandidateDigest, Supersedes: spec.Supersedes,
			State: workspacecoord.MergePending, Version: 1, CreatedAt: now, UpdatedAt: now,
			CorrelationID: spec.CorrelationID, IdempotencyKey: spec.IdempotencyKey,
		}
		if err := createRecord(tx, mergeRecordKind, string(id), merge); err != nil {
			return err
		}
		if spec.IdempotencyKey != "" {
			if err := createRecord(tx, "merge-key", workspaceIdentityID(spec.TeamID, spec.IdempotencyKey), workspaceIdentity{ID: string(id), Hash: hash}); err != nil {
				return err
			}
		}
		events := []orchestration.PendingEvent{{
			Type: orchestration.EventMergeQueued, ActorAgentID: spec.AgentID, CorrelationID: spec.CorrelationID, IdempotencyKey: spec.IdempotencyKey,
			Payload: orchestration.MustJSONPayload(map[string]any{"merge_id": id, "view_id": spec.ViewID, "base_revision": spec.BaseRevision,
				"patch_artifact": spec.PatchArtifact, "kind": kind, "candidate_digest": spec.CandidateDigest,
				"execution_id": spec.ExecutionID, "contract_hash": spec.ContractHash, "attempt": spec.Attempt, "supersedes": spec.Supersedes}),
		}}
		if spec.Supersedes != "" {
			event, err := mergeSupersedeTx(tx, spec, id, now)
			if err != nil {
				return err
			}
			events = append(events, event)
		}
		if err := workspaceAppendEvents(tx, spec.TeamID, events...); err != nil {
			return err
		}
		result = merge
		return nil
	})
	if err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	return result, nil
}

func mergeSupersedeTx(tx *ateauthority.Tx, spec workspacecoord.MergeSpec, id orchestration.MergeID, now time.Time) (orchestration.PendingEvent, error) {
	previous, err := getRecord[workspacecoord.MergeRequest](tx, mergeRecordKind, string(spec.Supersedes))
	if err != nil {
		return orchestration.PendingEvent{}, err
	}
	if previous.TeamID != spec.TeamID {
		return orchestration.PendingEvent{}, fmt.Errorf("%w: superseded merge belongs to another team", orchestration.ErrInvalidEvent)
	}
	sameTask := spec.TaskID != "" && previous.TaskID == spec.TaskID
	resolution := spec.TaskID != "" && previous.ResolutionTaskID == spec.TaskID && previous.State == workspacecoord.MergeConflicted
	sameUntaskedView := spec.TaskID == "" && previous.TaskID == "" && previous.ViewID == spec.ViewID && previous.AgentID == spec.AgentID
	if !sameTask && !resolution && !sameUntaskedView {
		return orchestration.PendingEvent{}, fmt.Errorf("%w: replacement is not a revision or resolution of the previous delivery", orchestration.ErrInvalidEvent)
	}
	if err := mergeValidateTransition(previous.State, workspacecoord.MergeSuperseded); err != nil {
		return orchestration.PendingEvent{}, err
	}
	from := previous.State
	previous.State, previous.SupersededBy = workspacecoord.MergeSuperseded, id
	previous.Reason, previous.UpdatedAt = "superseded by "+string(id), now
	previous.Version++
	if err := putRecord(tx, mergeRecordKind, string(previous.ID), previous); err != nil {
		return orchestration.PendingEvent{}, err
	}
	return orchestration.PendingEvent{
		Type: orchestration.EventMergeSuperseded, ActorAgentID: spec.AgentID, CorrelationID: spec.CorrelationID,
		Payload: orchestration.MustJSONPayload(map[string]any{"merge_id": previous.ID, "from": from, "superseded_by": id}),
	}, nil
}

func (store *Store) GetMerge(ctx context.Context, id orchestration.MergeID) (workspacecoord.MergeRequest, error) {
	if err := id.Validate(); err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	var result workspacecoord.MergeRequest
	err := store.authority.View(workspaceContext(ctx), func(tx *ateauthority.Tx) error {
		var err error
		result, err = getRecord[workspacecoord.MergeRequest](tx, mergeRecordKind, string(id))
		return err
	})
	return result, err
}

func (store *Store) ListMerges(ctx context.Context, query workspacecoord.MergeQuery) ([]workspacecoord.MergeRequest, error) {
	if err := query.TeamID.Validate(); err != nil {
		return nil, err
	}
	var result []workspacecoord.MergeRequest
	err := store.authority.View(workspaceContext(ctx), func(tx *ateauthority.Tx) error {
		var err error
		result, err = mergeListTx(tx, query)
		return err
	})
	return result, err
}

func mergeListTx(tx *ateauthority.Tx, query workspacecoord.MergeQuery) ([]workspacecoord.MergeRequest, error) {
	merges, err := listRecords[workspacecoord.MergeRequest](tx, mergeRecordKind)
	if err != nil {
		return nil, err
	}
	result := make([]workspacecoord.MergeRequest, 0)
	for _, merge := range merges {
		if merge.TeamID == query.TeamID && (len(query.States) == 0 || slices.Contains(query.States, merge.State)) &&
			(query.ViewID == "" || query.ViewID == merge.ViewID) && (query.TaskID == "" || query.TaskID == merge.TaskID) &&
			(query.ResolutionTaskID == "" || query.ResolutionTaskID == merge.ResolutionTaskID) {
			result = append(result, merge)
		}
	}
	slices.SortFunc(result, func(a, b workspacecoord.MergeRequest) int {
		if order := a.CreatedAt.Compare(b.CreatedAt); order != 0 {
			return order
		}
		return strings.Compare(string(a.ID), string(b.ID))
	})
	return result, nil
}

func (store *Store) TransitionMerge(ctx context.Context, transition workspacecoord.MergeTransition) (workspacecoord.MergeRequest, error) {
	if err := transition.MergeID.Validate(); err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	if transition.ActorAgentID != "" {
		if err := transition.ActorAgentID.Validate(); err != nil {
			return workspacecoord.MergeRequest{}, err
		}
	}
	now := time.Now().UTC()
	var result workspacecoord.MergeRequest
	err := store.authority.Update(workspaceContext(ctx), func(tx *ateauthority.Tx) error {
		merge, err := getRecord[workspacecoord.MergeRequest](tx, mergeRecordKind, string(transition.MergeID))
		if err != nil {
			return err
		}
		if merge.State == workspacecoord.MergeConflicted && transition.Target == workspacecoord.MergePending {
			if transition.ResolutionTaskID == "" || merge.ResolutionTaskID != transition.ResolutionTaskID {
				return fmt.Errorf("%w: merge resolution task is required", orchestration.ErrInvalidEvent)
			}
			task, err := getRecord[orchestration.TaskSnapshot](tx, "task", string(transition.ResolutionTaskID))
			if err != nil {
				return err
			}
			if task.TeamID != merge.TeamID || task.State != orchestration.TaskCompleted {
				return fmt.Errorf("%w: resolution task is not completed", orchestration.ErrInvalidEvent)
			}
		}
		if transition.Expected != 0 && merge.Version != transition.Expected {
			return fmt.Errorf("%w: merge expected version %d, current %d", orchestration.ErrSequenceConflict, transition.Expected, merge.Version)
		}
		if err := mergeValidateTransition(merge.State, transition.Target); err != nil {
			return err
		}
		from := merge.State
		merge.State, merge.Reason, merge.UpdatedAt = transition.Target, transition.Reason, now
		merge.Version++
		if transition.PlanDigest != "" {
			merge.PlanDigest = transition.PlanDigest
		}
		if err := putRecord(tx, mergeRecordKind, string(merge.ID), merge); err != nil {
			return err
		}
		eventType := orchestration.EventMergeStarted
		if from == workspacecoord.MergeConflicted && transition.Target == workspacecoord.MergePending {
			eventType = orchestration.EventMergeRequeued
		}
		switch transition.Target {
		case workspacecoord.MergeApplied:
			eventType = orchestration.EventMergeApplied
		case workspacecoord.MergeConflicted:
			eventType = orchestration.EventMergeConflicted
		case workspacecoord.MergeRejected:
			eventType = orchestration.EventMergeRejected
		case workspacecoord.MergeSuperseded:
			eventType = orchestration.EventMergeSuperseded
		}
		payload := map[string]any{"merge_id": merge.ID, "from": from, "to": transition.Target, "reason": transition.Reason}
		if merge.PlanDigest != "" {
			payload["plan_digest"] = merge.PlanDigest
		}
		if transition.ResolutionTaskID != "" {
			payload["resolution_task_id"] = transition.ResolutionTaskID
		}
		if err := workspaceAppendEvents(tx, merge.TeamID, orchestration.PendingEvent{
			Type: eventType, ActorAgentID: transition.ActorAgentID, CorrelationID: transition.CorrelationID,
			Payload: orchestration.MustJSONPayload(payload),
		}); err != nil {
			return err
		}
		result = merge
		return nil
	})
	if err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	return result, nil
}

func (store *Store) LinkMergeResolutionTask(ctx context.Context, id orchestration.MergeID, taskID orchestration.TaskID) (workspacecoord.MergeRequest, error) {
	if err := id.Validate(); err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	if err := taskID.Validate(); err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	var result workspacecoord.MergeRequest
	err := store.authority.Update(workspaceContext(ctx), func(tx *ateauthority.Tx) error {
		merge, err := getRecord[workspacecoord.MergeRequest](tx, mergeRecordKind, string(id))
		if err != nil {
			return err
		}
		if merge.ResolutionTaskID == taskID {
			result = merge
			return nil
		}
		if merge.State != workspacecoord.MergeConflicted || merge.ResolutionTaskID != "" {
			return fmt.Errorf("%w: merge resolution task is already linked", orchestration.ErrIdempotencyConflict)
		}
		if err := workspaceTaskTeam(tx, taskID, merge.TeamID); err != nil {
			return err
		}
		// As in SQL, linking does not change the queue's version or timestamp.
		merge.ResolutionTaskID = taskID
		if err := putRecord(tx, mergeRecordKind, string(id), merge); err != nil {
			return err
		}
		if err := workspaceAppendEvents(tx, merge.TeamID, orchestration.PendingEvent{
			Type: orchestration.EventMergeResolutionLinked, ActorAgentID: merge.AgentID, CorrelationID: merge.CorrelationID,
			Payload: orchestration.MustJSONPayload(map[string]any{"merge_id": id, "resolution_task_id": taskID}),
		}); err != nil {
			return err
		}
		result = merge
		return nil
	})
	if err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	return result, nil
}

func mergeValidateTransition(from, to workspacecoord.MergeState) error {
	valid := false
	switch from {
	case workspacecoord.MergePending:
		valid = to == workspacecoord.MergeRunning || to == workspacecoord.MergeRejected || to == workspacecoord.MergeSuperseded
	case workspacecoord.MergeRunning:
		valid = to == workspacecoord.MergeApplied || to == workspacecoord.MergeConflicted || to == workspacecoord.MergeRejected
	case workspacecoord.MergeConflicted:
		valid = to == workspacecoord.MergePending || to == workspacecoord.MergeRejected || to == workspacecoord.MergeSuperseded
	}
	if !valid {
		return fmt.Errorf("%w: invalid merge transition %s -> %s", orchestration.ErrInvalidEvent, from, to)
	}
	return nil
}

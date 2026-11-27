package atestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

const mergeApplyKind = "merge-apply"

// Progress is stored separately from the immutable plan, one record per path.
// Reading a journal assembles a fresh result; Store retains no projection.
type mergeApplyJournal struct {
	MergeID   orchestration.MergeID     `json:"merge_id"`
	Plan      workspacecoord.ApplyPlan  `json:"plan"`
	State     workspacecoord.ApplyState `json:"state"`
	CreatedAt time.Time                 `json:"created_at"`
	UpdatedAt time.Time                 `json:"updated_at"`
}

type mergeApplyPath struct {
	Path      string    `json:"path"`
	AppliedAt time.Time `json:"applied_at"`
}

func mergeApplyPathKind(id orchestration.MergeID) string {
	return "merge-apply-path/" + string(id)
}

func (store *Store) BeginApply(ctx context.Context, plan workspacecoord.ApplyPlan) (workspacecoord.ApplyRecord, error) {
	if store == nil || store.authority == nil {
		return workspacecoord.ApplyRecord{}, errors.New("orchestration store is unavailable")
	}
	// Freeze caller-owned slices before entering a callback that may be retried.
	encoded, err := json.Marshal(plan)
	if err != nil {
		return workspacecoord.ApplyRecord{}, fmt.Errorf("encode apply plan: %w", err)
	}
	var saved workspacecoord.ApplyPlan
	if err := json.Unmarshal(encoded, &saved); err != nil {
		return workspacecoord.ApplyRecord{}, err
	}
	now := time.Now().UTC()
	var result workspacecoord.ApplyRecord
	err = store.authority.Update(workspaceContext(ctx), func(tx *ateauthority.Tx) error {
		existing, found, err := mergeLoadApplyTx(tx, saved.MergeID)
		if err != nil {
			return err
		}
		if found {
			if existing.Plan.Digest != saved.Digest {
				return fmt.Errorf("%w: merge %s already journals another plan", orchestration.ErrInvalidEvent, saved.MergeID)
			}
			result = existing
			return nil
		}
		merge, err := getRecord[workspacecoord.MergeRequest](tx, mergeRecordKind, string(saved.MergeID))
		if err != nil {
			return err
		}
		if merge.State != workspacecoord.MergeRunning {
			return fmt.Errorf("%w: merge %s is %s, not running", orchestration.ErrInvalidEvent, saved.MergeID, merge.State)
		}
		journal := mergeApplyJournal{MergeID: saved.MergeID, Plan: saved, State: workspacecoord.ApplyStarted, CreatedAt: now, UpdatedAt: now}
		if err := createRecord(tx, mergeApplyKind, string(saved.MergeID), journal); err != nil {
			return err
		}
		result = workspacecoord.ApplyRecord{MergeID: saved.MergeID, Plan: saved, State: workspacecoord.ApplyStarted, Applied: map[string]bool{}}
		return nil
	})
	if err != nil {
		return workspacecoord.ApplyRecord{}, err
	}
	return result, nil
}

func (store *Store) MarkPathApplied(ctx context.Context, id orchestration.MergeID, path string) error {
	now := time.Now().UTC()
	return store.authority.Update(workspaceContext(ctx), func(tx *ateauthority.Tx) error {
		journal, err := getRecord[mergeApplyJournal](tx, mergeApplyKind, string(id))
		if err != nil {
			return err
		}
		if journal.State != workspacecoord.ApplyStarted {
			return fmt.Errorf("%w: apply journal of merge %s is %s", orchestration.ErrInvalidEvent, id, journal.State)
		}
		_, err = getRecord[mergeApplyPath](tx, mergeApplyPathKind(id), workspaceDigest(path))
		if err == nil {
			return nil
		}
		if !errors.Is(err, orchestration.ErrNotFound) {
			return err
		}
		return createRecord(tx, mergeApplyPathKind(id), workspaceDigest(path), mergeApplyPath{Path: path, AppliedAt: now})
	})
}

func (store *Store) FinishApply(ctx context.Context, id orchestration.MergeID) error {
	now := time.Now().UTC()
	return store.authority.Update(workspaceContext(ctx), func(tx *ateauthority.Tx) error {
		journal, err := getRecord[mergeApplyJournal](tx, mergeApplyKind, string(id))
		if err != nil {
			return err
		}
		journal.State, journal.UpdatedAt = workspacecoord.ApplyFinished, now
		return putRecord(tx, mergeApplyKind, string(id), journal)
	})
}

// LoadApply reads the stored apply journal entry for a merge, reporting whether it exists.
func (store *Store) LoadApply(ctx context.Context, id orchestration.MergeID) (workspacecoord.ApplyRecord, bool, error) {
	var result workspacecoord.ApplyRecord
	var found bool
	err := store.authority.View(workspaceContext(ctx), func(tx *ateauthority.Tx) error {
		var err error
		result, found, err = mergeLoadApplyTx(tx, id)
		return err
	})
	if err != nil {
		return workspacecoord.ApplyRecord{}, false, err
	}
	return result, found, nil
}

func mergeLoadApplyTx(tx *ateauthority.Tx, id orchestration.MergeID) (workspacecoord.ApplyRecord, bool, error) {
	journal, err := getRecord[mergeApplyJournal](tx, mergeApplyKind, string(id))
	if errors.Is(err, orchestration.ErrNotFound) {
		return workspacecoord.ApplyRecord{}, false, nil
	}
	if err != nil {
		return workspacecoord.ApplyRecord{}, false, err
	}
	paths, err := listRecords[mergeApplyPath](tx, mergeApplyPathKind(id))
	if err != nil {
		return workspacecoord.ApplyRecord{}, false, err
	}
	record := workspacecoord.ApplyRecord{MergeID: id, Plan: journal.Plan, State: journal.State, Applied: make(map[string]bool, len(paths))}
	for _, path := range paths {
		record.Applied[path.Path] = true
	}
	return record, true, nil
}

var _ workspacecoord.ApplyJournal = (*Store)(nil)

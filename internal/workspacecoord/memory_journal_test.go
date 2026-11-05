package workspacecoord

import (
	"context"
	"fmt"
	"maps"
	"sync"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

// MemoryApplyJournal is an in-process ApplyJournal for tests.
type MemoryApplyJournal struct {
	mutex   sync.Mutex
	records map[orchestration.MergeID]ApplyRecord
}

// NewMemoryApplyJournal returns an empty in-process journal.
func NewMemoryApplyJournal() *MemoryApplyJournal {
	return &MemoryApplyJournal{records: make(map[orchestration.MergeID]ApplyRecord)}
}

// BeginApply records plan unless the merge already has a journal.
func (journal *MemoryApplyJournal) BeginApply(_ context.Context, plan ApplyPlan) (ApplyRecord, error) {
	journal.mutex.Lock()
	defer journal.mutex.Unlock()
	if record, ok := journal.records[plan.MergeID]; ok {
		if record.Plan.Digest != plan.Digest {
			return ApplyRecord{}, fmt.Errorf("%w: merge %s already journals another plan", orchestration.ErrInvalidEvent, plan.MergeID)
		}
		return cloneRecord(record), nil
	}
	record := ApplyRecord{MergeID: plan.MergeID, Plan: plan, State: ApplyStarted, Applied: map[string]bool{}}
	journal.records[plan.MergeID] = record
	return cloneRecord(record), nil
}

// MarkPathApplied records one committed path.
func (journal *MemoryApplyJournal) MarkPathApplied(_ context.Context, mergeID orchestration.MergeID, path string) error {
	journal.mutex.Lock()
	defer journal.mutex.Unlock()
	record, ok := journal.records[mergeID]
	if !ok {
		return fmt.Errorf("%w: merge %s has no apply journal", orchestration.ErrNotFound, mergeID)
	}
	record.Applied[path] = true
	return nil
}

// FinishApply marks the commit done.
func (journal *MemoryApplyJournal) FinishApply(_ context.Context, mergeID orchestration.MergeID) error {
	journal.mutex.Lock()
	defer journal.mutex.Unlock()
	record, ok := journal.records[mergeID]
	if !ok {
		return fmt.Errorf("%w: merge %s has no apply journal", orchestration.ErrNotFound, mergeID)
	}
	record.State = ApplyFinished
	journal.records[mergeID] = record
	return nil
}

// LoadApply returns the journal of a merge, if any.
func (journal *MemoryApplyJournal) LoadApply(_ context.Context, mergeID orchestration.MergeID) (ApplyRecord, bool, error) {
	journal.mutex.Lock()
	defer journal.mutex.Unlock()
	record, ok := journal.records[mergeID]
	return cloneRecord(record), ok, nil
}

// cloneRecord copies the applied set so callers never share the map.
func cloneRecord(record ApplyRecord) ApplyRecord {
	applied := make(map[string]bool, len(record.Applied))
	maps.Copy(applied, record.Applied)
	record.Applied = applied
	return record
}

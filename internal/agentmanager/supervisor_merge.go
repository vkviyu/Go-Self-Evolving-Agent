package agentmanager

import (
	"context"
	"errors"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

// RunPendingMerges executes queued workspace merges through the configured
// serialized merge executor. It is intentionally explicit: callers decide
// when a merge worker should spend filesystem and Git resources.
func (supervisor *Supervisor) RunPendingMerges(ctx context.Context, teamID orchestration.TeamID, limit int) ([]workspacecoord.MergeRequest, error) {
	if err := supervisor.ensureOpen(); err != nil {
		return nil, err
	}
	if supervisor.mergeExecutor == nil {
		return nil, errors.New("merge executor is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	merges, err := supervisor.mergeExecutor.RunPending(ctx, teamID, limit)
	if err == nil {
		supervisor.signal()
	}
	return merges, err
}

// RequeueResolvedMerge explicitly retries a conflicted merge after its linked
// resolution task has completed. The executor validates the task association.
func (supervisor *Supervisor) RequeueResolvedMerge(ctx context.Context, mergeID orchestration.MergeID, taskID orchestration.TaskID) (workspacecoord.MergeRequest, error) {
	if err := supervisor.ensureOpen(); err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	if supervisor.mergeExecutor == nil {
		return workspacecoord.MergeRequest{}, errors.New("merge executor is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	merge, err := supervisor.mergeExecutor.RequeueResolved(ctx, mergeID, taskID)
	if err == nil {
		supervisor.signal()
	}
	return merge, err
}

// ReviewMerges starts a background pass over the Team's queued snapshot
// merges, so each asks the approver again. It returns at once: approval may
// wait for the user.
func (supervisor *Supervisor) ReviewMerges(teamID orchestration.TeamID) error {
	if err := supervisor.ensureOpen(); err != nil {
		return err
	}
	if supervisor.mergeExecutor == nil {
		return errors.New("merge executor is unavailable")
	}
	if err := teamID.Validate(); err != nil {
		return err
	}
	supervisor.scheduleMerges(teamID)
	return nil
}

// DiscardMerge rejects an open merge and fails its task, then lets work that
// waited on the task proceed.
func (supervisor *Supervisor) DiscardMerge(ctx context.Context, mergeID orchestration.MergeID, reason string) (workspacecoord.MergeRequest, error) {
	if err := supervisor.ensureOpen(); err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	if supervisor.mergeExecutor == nil {
		return workspacecoord.MergeRequest{}, errors.New("merge executor is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	merge, err := supervisor.mergeExecutor.Discard(ctx, mergeID, reason)
	if merge.TeamID != "" {
		supervisor.afterMerges(merge.TeamID)
	}
	return merge, err
}

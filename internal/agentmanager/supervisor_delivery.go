package agentmanager

import (
	"context"
	"fmt"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

// deliverWork queues the output of a successful run for merge: the candidate
// of a snapshot view or the patch of a git worktree view. It reports false
// when the Agent has no private view or left it unchanged. task is the zero
// task for unassigned Agents.
func (supervisor *Supervisor) deliverWork(ctx context.Context, agent orchestration.AgentSnapshot, task orchestration.TaskSnapshot, result *orchestration.AgentResult) (bool, error) {
	if supervisor.workspace == nil || supervisor.workspace.Repository == nil || result.Status != orchestration.ResultSucceeded {
		return false, nil
	}
	view, ok, err := supervisor.currentView(ctx, agent)
	if err != nil || !ok {
		return false, err
	}
	switch view.Mode {
	case workspacecoord.ViewSnapshot:
		return supervisor.deliverSnapshot(ctx, agent, task, view, result)
	case workspacecoord.ViewGitWorktree:
		return supervisor.deliverPatch(ctx, agent, task, view, result)
	default:
		return false, nil
	}
}

// deliverSnapshot freezes the snapshot view of a successful run and queues
// its candidate for merge. It reports false when the run left the view
// unchanged.
func (supervisor *Supervisor) deliverSnapshot(ctx context.Context, agent orchestration.AgentSnapshot, task orchestration.TaskSnapshot, view workspacecoord.View, result *orchestration.AgentResult) (bool, error) {
	if supervisor.workspace.Snapshots == nil {
		return false, fmt.Errorf("deliver snapshot view %s: snapshot store is unavailable", view.ID)
	}
	snapshot, err := supervisor.workspace.Snapshots.ForView(view)
	if err != nil {
		return false, fmt.Errorf("open snapshot of view %s: %w", view.ID, err)
	}
	candidate, changed, err := workspacecoord.BuildCandidate(ctx, snapshot, view.ID, supervisor.workspace.Artifacts)
	if err != nil {
		return false, fmt.Errorf("build candidate of view %s: %w", view.ID, err)
	}
	if !changed {
		return false, nil
	}
	merge, found, err := supervisor.queuedCandidate(ctx, agent, view, candidate.Digest)
	if err != nil {
		return false, err
	}
	if !found {
		if merge, err = supervisor.enqueueCandidate(ctx, agent, task, view, candidate); err != nil {
			return false, err
		}
	}
	result.Delivery = &orchestration.WorkspaceDelivery{
		ViewID: view.ID, CandidateDigest: candidate.Digest, MergeID: merge.ID, ChangedPaths: len(candidate.Entries),
	}
	return true, nil
}

// currentView returns the view of the Agent's current run. After a restart
// the in-memory record is gone, so the latest active view is used.
func (supervisor *Supervisor) currentView(ctx context.Context, agent orchestration.AgentSnapshot) (workspacecoord.View, bool, error) {
	supervisor.mu.Lock()
	viewID := supervisor.workspaceViews[agent.ID]
	supervisor.mu.Unlock()
	if viewID != "" {
		view, err := supervisor.workspace.Repository.GetView(ctx, viewID)
		if err != nil {
			return workspacecoord.View{}, false, fmt.Errorf("load workspace view %s: %w", viewID, err)
		}
		return view, view.State == workspacecoord.ViewActive, nil
	}
	views, err := supervisor.workspace.Repository.ListViews(ctx, workspacecoord.ViewQuery{
		TeamID: agent.TeamID, AgentID: agent.ID, States: []workspacecoord.ViewState{workspacecoord.ViewActive},
	})
	if err != nil {
		return workspacecoord.View{}, false, fmt.Errorf("list Agent workspace views: %w", err)
	}
	if len(views) == 0 {
		return workspacecoord.View{}, false, nil
	}
	return views[len(views)-1], true, nil
}

// queuedCandidate finds the merge this run already queued for the candidate,
// so a retried finish reuses it instead of superseding it. A later run of
// the same view queues the candidate again under its own key.
func (supervisor *Supervisor) queuedCandidate(ctx context.Context, agent orchestration.AgentSnapshot, view workspacecoord.View, digest string) (workspacecoord.MergeRequest, bool, error) {
	merges, err := supervisor.workspace.Repository.ListMerges(ctx, workspacecoord.MergeQuery{TeamID: view.TeamID, ViewID: view.ID})
	if err != nil {
		return workspacecoord.MergeRequest{}, false, fmt.Errorf("list merges of view %s: %w", view.ID, err)
	}
	for _, merge := range merges {
		if merge.Kind == workspacecoord.MergeSnapshot && merge.CandidateDigest == digest && merge.CorrelationID == string(agent.CurrentRunID) {
			return merge, true, nil
		}
	}
	return workspacecoord.MergeRequest{}, false, nil
}

// enqueueCandidate stores the candidate and queues its merge. The merge
// replaces the conflicted merge the task resolves, or an older open
// candidate of the same task.
func (supervisor *Supervisor) enqueueCandidate(ctx context.Context, agent orchestration.AgentSnapshot, task orchestration.TaskSnapshot, view workspacecoord.View, candidate workspacecoord.Candidate) (workspacecoord.MergeRequest, error) {
	reference, err := workspacecoord.StoreCandidate(ctx, supervisor.workspace.Artifacts, candidate)
	if err != nil {
		return workspacecoord.MergeRequest{}, fmt.Errorf("store candidate of view %s: %w", view.ID, err)
	}
	supersedes, err := supervisor.supersededMerge(ctx, task)
	if err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	merge, err := supervisor.workspace.Repository.EnqueueMerge(ctx, workspacecoord.MergeSpec{
		TeamID: agent.TeamID, AgentID: agent.ID, TaskID: task.ID, ViewID: view.ID,
		BaseRevision: view.BaseRevision, PatchArtifact: reference, Kind: workspacecoord.MergeSnapshot,
		CandidateDigest: candidate.Digest, Supersedes: supersedes, CorrelationID: string(agent.CurrentRunID),
		IdempotencyKey: "merge:" + string(view.ID) + ":" + string(agent.CurrentRunID) + ":" + candidate.Digest,
	})
	if err != nil {
		return workspacecoord.MergeRequest{}, fmt.Errorf("queue candidate of view %s: %w", view.ID, err)
	}
	return merge, nil
}

// supersededMerge names the open merge a new candidate of task replaces.
// Running merges are never replaced; the executor settles them first.
func (supervisor *Supervisor) supersededMerge(ctx context.Context, task orchestration.TaskSnapshot) (orchestration.MergeID, error) {
	if task.ID == "" {
		return "", nil
	}
	resolved, err := supervisor.latestSnapshotMerge(ctx, workspacecoord.MergeQuery{
		TeamID: task.TeamID, ResolutionTaskID: task.ID, States: []workspacecoord.MergeState{workspacecoord.MergeConflicted},
	})
	if err != nil || resolved.ID != "" {
		return resolved.ID, err
	}
	previous, err := supervisor.latestSnapshotMerge(ctx, workspacecoord.MergeQuery{
		TeamID: task.TeamID, TaskID: task.ID, States: []workspacecoord.MergeState{workspacecoord.MergePending, workspacecoord.MergeConflicted},
	})
	return previous.ID, err
}

// deliverPatch stores the patch of a git worktree view against its base and
// queues it as a git-patch merge. A later run of the same worktree diffs
// from the same base, so its patch replaces the view's pending merge. It
// reports false when the worktree has no output.
func (supervisor *Supervisor) deliverPatch(ctx context.Context, agent orchestration.AgentSnapshot, task orchestration.TaskSnapshot, view workspacecoord.View, result *orchestration.AgentResult) (bool, error) {
	if supervisor.workspace.Worktrees == nil || supervisor.workspace.Artifacts == nil {
		return false, fmt.Errorf("deliver worktree view %s: git worktree support is unavailable", view.ID)
	}
	patch, err := supervisor.workspace.Worktrees.Diff(ctx, view)
	if err != nil {
		return false, fmt.Errorf("capture patch of view %s: %w", view.ID, err)
	}
	if len(patch) == 0 {
		return false, nil
	}
	reference, err := supervisor.workspace.Artifacts.Put(ctx, patch)
	if err != nil {
		return false, fmt.Errorf("store patch of view %s: %w", view.ID, err)
	}
	merges, err := supervisor.workspace.Repository.ListMerges(ctx, workspacecoord.MergeQuery{TeamID: view.TeamID, ViewID: view.ID})
	if err != nil {
		return false, fmt.Errorf("list merges of view %s: %w", view.ID, err)
	}
	var merge, supersedes workspacecoord.MergeRequest
	for _, existing := range merges {
		switch {
		case existing.Kind != workspacecoord.MergeGitPatch:
		case existing.CorrelationID == string(agent.CurrentRunID) && existing.PatchArtifact == reference:
			merge = existing
		case existing.State == workspacecoord.MergePending:
			supersedes = existing
		}
	}
	if merge.ID == "" {
		merge, err = supervisor.workspace.Repository.EnqueueMerge(ctx, workspacecoord.MergeSpec{
			TeamID: agent.TeamID, AgentID: agent.ID, TaskID: task.ID, ViewID: view.ID,
			BaseRevision: view.BaseRevision, PatchArtifact: reference, Kind: workspacecoord.MergeGitPatch,
			Supersedes: supersedes.ID, CorrelationID: string(agent.CurrentRunID),
			IdempotencyKey: "merge:" + string(view.ID) + ":" + string(agent.CurrentRunID) + ":" + reference,
		})
		if err != nil {
			return false, fmt.Errorf("queue patch of view %s: %w", view.ID, err)
		}
	}
	result.Delivery = &orchestration.WorkspaceDelivery{
		ViewID: view.ID, CandidateDigest: reference, MergeID: merge.ID, ChangedPaths: len(workspacecoord.PatchPaths(patch)),
	}
	return true, nil
}

// scheduleMerges runs the Team's background merges. A call during a pass
// asks that pass to run once more, so passes never overlap.
func (supervisor *Supervisor) scheduleMerges(teamID orchestration.TeamID) {
	if supervisor.mergeExecutor == nil || teamID == "" {
		return
	}
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if supervisor.closed {
		return
	}
	if _, running := supervisor.mergeRuns[teamID]; running {
		supervisor.mergeRuns[teamID] = true
		return
	}
	supervisor.mergeRuns[teamID] = false
	supervisor.workers.Go(func() {
		supervisor.runMerges(teamID)
	})
}

// runMerges repeats merge passes until no rerun is requested. Merges that
// fail stay durable and are retried by the next pass or by recovery.
func (supervisor *Supervisor) runMerges(teamID orchestration.TeamID) {
	for {
		if supervisor.teamAllowsExecution(supervisor.ctx, teamID) {
			_, _ = supervisor.mergeExecutor.RunPendingAutomatic(supervisor.ctx, teamID, 0)
		}
		supervisor.afterMerges(teamID)
		supervisor.mu.Lock()
		rerun := supervisor.mergeRuns[teamID] && !supervisor.closed && supervisor.ctx.Err() == nil
		if !rerun {
			delete(supervisor.mergeRuns, teamID)
			supervisor.mu.Unlock()
			return
		}
		supervisor.mergeRuns[teamID] = false
		supervisor.mu.Unlock()
	}
}

// afterMerges starts work a merge pass unlocked: dependents of completed
// tasks, merge resolution tasks and parents waiting on those tasks.
func (supervisor *Supervisor) afterMerges(teamID orchestration.TeamID) {
	if supervisor.ctx.Err() != nil {
		return
	}
	if supervisor.runner != nil && supervisor.tasks != nil {
		_, _ = supervisor.dispatchReadyTasks(supervisor.ctx, teamID)
	}
	if supervisor.waitSets != nil {
		_, _ = supervisor.ReconcileWaitSets(supervisor.ctx, teamID)
	}
	supervisor.signal()
	supervisor.startAvailable()
}

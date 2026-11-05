package agentmanager

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentprofile"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

func (supervisor *Supervisor) startWorkspaceView(ctx context.Context, agent orchestration.AgentSnapshot) error {
	if supervisor.workspace == nil {
		return nil
	}
	if supervisor.workspace.Repository == nil {
		return errors.New("workspace coordinator repository is unavailable")
	}
	if strings.TrimSpace(supervisor.workspaceRoot) == "" {
		return errors.New("workspace root is required when workspace coordination is enabled")
	}
	views, err := supervisor.workspace.Repository.ListViews(ctx, workspacecoord.ViewQuery{
		TeamID: agent.TeamID, AgentID: agent.ID, States: []workspacecoord.ViewState{workspacecoord.ViewActive},
	})
	if err != nil {
		return fmt.Errorf("list Agent workspace views: %w", err)
	}
	if len(views) > 0 {
		view := views[len(views)-1]
		reuse, err := supervisor.reusableView(ctx, agent, view)
		if err != nil {
			return err
		}
		if reuse {
			supervisor.rememberView(agent.ID, view.ID)
			return nil
		}
	}
	mode := viewMode(supervisor.workspaceMode(agent))
	spec := workspacecoord.ViewSpec{
		TeamID: agent.TeamID, AgentID: agent.ID, TaskID: agent.AssignedTaskID, Mode: mode,
		WorkspaceRoot: supervisor.workspaceRoot, RootPath: supervisor.workspaceRoot,
		CorrelationID: string(agent.CurrentRunID), IdempotencyKey: "view:" + string(agent.ID) + ":" + string(agent.CurrentRunID),
	}
	source, err := supervisor.viewSource(ctx, agent)
	if err != nil {
		return err
	}
	switch {
	case source.ID != "" && agent.AssignedTaskID == "":
		return supervisor.startReviewView(ctx, agent, spec, source)
	case source.ID != "" || mode == workspacecoord.ViewSnapshot:
		if supervisor.workspace.Snapshots == nil {
			return errors.New("snapshot workspace support is unavailable")
		}
		return supervisor.startSnapshotView(ctx, agent, spec, source)
	}
	if mode == workspacecoord.ViewGitWorktree {
		if supervisor.workspace.Worktrees == nil {
			return errors.New("git worktree manager is unavailable")
		}
		baseRevision, err := supervisor.workspace.Worktrees.ResolveHead(ctx)
		if err != nil {
			return fmt.Errorf("resolve workspace base revision: %w", err)
		}
		spec.BaseRevision = baseRevision
		spec.RootPath = workspacecoord.DefaultWorktreePath(supervisor.workspaceRoot, orchestration.WorkspaceViewID(string(agent.ID)))
	}
	view, err := supervisor.workspace.CreateView(ctx, spec)
	if err != nil {
		return fmt.Errorf("create Agent workspace view: %w", err)
	}
	supervisor.rememberView(agent.ID, view.ID)
	return nil
}

// rememberView records the view the Agent's current run works in.
func (supervisor *Supervisor) rememberView(agentID orchestration.AgentID, viewID orchestration.WorkspaceViewID) {
	supervisor.mu.Lock()
	supervisor.workspaceViews[agentID] = viewID
	supervisor.mu.Unlock()
}

// reusableView reports whether a run continues in the Agent's latest active
// view. A snapshot that delivered work to an open merge belongs to that merge,
// so a later run starts a new snapshot instead of editing it.
func (supervisor *Supervisor) reusableView(ctx context.Context, agent orchestration.AgentSnapshot, view workspacecoord.View) (bool, error) {
	if (view.Mode != workspacecoord.ViewSnapshot && view.Mode != workspacecoord.ViewGitWorktree) || view.CorrelationID == string(agent.CurrentRunID) {
		return true, nil
	}
	merges, err := supervisor.workspace.Repository.ListMerges(ctx, workspacecoord.MergeQuery{
		TeamID: agent.TeamID, ViewID: view.ID, States: openMergeStates(),
	})
	if err != nil {
		return false, fmt.Errorf("list merges of workspace view %s: %w", view.ID, err)
	}
	if view.Mode == workspacecoord.ViewGitWorktree && len(merges) > 0 {
		return false, fmt.Errorf("%w: worktree %s is frozen for pending merge/review; finish or discard its merge before continuing", orchestration.ErrInvalidEvent, view.ID)
	}
	return len(merges) == 0, nil
}

// ensureContinuableWorkspace rejects a new continuation before its mailbox
// directive and run transition are committed. A Git reviewer sees the retained
// worktree, so editing that worktree while its immutable patch is pending would
// invalidate what the reviewer actually inspected.
func (supervisor *Supervisor) ensureContinuableWorkspace(ctx context.Context, agent orchestration.AgentSnapshot) error {
	if supervisor.workspace == nil || supervisor.workspace.Repository == nil {
		return nil
	}
	view, found, err := supervisor.currentView(ctx, agent)
	if err != nil || !found || view.Mode != workspacecoord.ViewGitWorktree {
		return err
	}
	merges, err := supervisor.workspace.Repository.ListMerges(ctx, workspacecoord.MergeQuery{
		TeamID: agent.TeamID, ViewID: view.ID, States: openMergeStates(),
	})
	if err != nil {
		return err
	}
	if len(merges) > 0 {
		return fmt.Errorf("%w: worktree %s is frozen for pending merge/review; finish or discard its merge before continuing", orchestration.ErrInvalidEvent, view.ID)
	}
	return nil
}

// startSnapshotView materializes the run's snapshot. A resolution task's
// snapshot starts from the conflicted candidate in source, replayed onto the
// current main workspace.
func (supervisor *Supervisor) startSnapshotView(ctx context.Context, agent orchestration.AgentSnapshot, spec workspacecoord.ViewSpec, source workspacecoord.MergeRequest) error {
	request := workspacecoord.SnapshotRequest{TeamID: agent.TeamID, AgentID: agent.ID, RunID: agent.CurrentRunID}
	if source.ID != "" {
		store, err := supervisor.artifactStore()
		if err != nil {
			return err
		}
		candidate, err := workspacecoord.LoadCandidate(ctx, store, source.PatchArtifact)
		if err != nil {
			return fmt.Errorf("load candidate of merge %s: %w", source.ID, err)
		}
		request.Prepare = workspacecoord.PrepareResolution(ctx, candidate, store)
	}
	view, err := supervisor.workspace.CreateSnapshotView(ctx, spec, request)
	if err != nil {
		return fmt.Errorf("create Agent snapshot view: %w", err)
	}
	supervisor.rememberView(agent.ID, view.ID)
	return nil
}

// startReviewView gives a reviewer read-only access to the work tree of the
// candidate or worktree patch it reviews, so the review sees exactly what
// will be merged.
func (supervisor *Supervisor) startReviewView(ctx context.Context, agent orchestration.AgentSnapshot, spec workspacecoord.ViewSpec, source workspacecoord.MergeRequest) error {
	reviewed, err := supervisor.workspace.Repository.GetView(ctx, source.ViewID)
	if err != nil {
		return fmt.Errorf("load reviewed workspace view %s: %w", source.ViewID, err)
	}
	spec.Mode, spec.RootPath, spec.BaseRevision = workspacecoord.ViewSharedReadonly, reviewed.RootPath, reviewed.BaseRevision
	view, err := supervisor.workspace.CreateView(ctx, spec)
	if err != nil {
		return fmt.Errorf("create Agent review view: %w", err)
	}
	supervisor.rememberView(agent.ID, view.ID)
	return nil
}

// viewSource returns the merge whose work the Agent starts from: the
// conflicted snapshot merge its task resolves, or the pending merge of the
// task it reviews. The zero merge means the Agent starts from main.
func (supervisor *Supervisor) viewSource(ctx context.Context, agent orchestration.AgentSnapshot) (workspacecoord.MergeRequest, error) {
	if agent.AssignedTaskID != "" {
		if supervisor.workspace.Snapshots == nil {
			return workspacecoord.MergeRequest{}, nil
		}
		return supervisor.latestSnapshotMerge(ctx, workspacecoord.MergeQuery{
			TeamID: agent.TeamID, ResolutionTaskID: agent.AssignedTaskID, States: []workspacecoord.MergeState{workspacecoord.MergeConflicted},
		})
	}
	task, ok, err := supervisor.reviewedTask(ctx, agent)
	if err != nil || !ok {
		return workspacecoord.MergeRequest{}, err
	}
	return supervisor.latestMerge(ctx, workspacecoord.MergeQuery{
		TeamID: agent.TeamID, TaskID: task.ID, States: []workspacecoord.MergeState{workspacecoord.MergePending},
	})
}

// reviewedTask finds the task in verification whose reviewer is agent. The
// reviewer is the child spawned under the task's review idempotency key.
func (supervisor *Supervisor) reviewedTask(ctx context.Context, agent orchestration.AgentSnapshot) (orchestration.TaskSnapshot, bool, error) {
	if supervisor.tasks == nil {
		return orchestration.TaskSnapshot{}, false, nil
	}
	tasks, err := supervisor.tasks.ListTasks(ctx, orchestration.TaskQuery{TeamID: agent.TeamID, States: []orchestration.TaskState{orchestration.TaskVerification}})
	if err != nil {
		return orchestration.TaskSnapshot{}, false, fmt.Errorf("list tasks in verification: %w", err)
	}
	for _, task := range tasks {
		reviewer, found, err := supervisor.agents.FindSpawn(ctx, agent.TeamID, reviewKey(task))
		if err != nil {
			return orchestration.TaskSnapshot{}, false, fmt.Errorf("find reviewer of task %s: %w", task.ID, err)
		}
		if found && reviewer.ID == agent.ID {
			return task, true, nil
		}
	}
	return orchestration.TaskSnapshot{}, false, nil
}

// latestSnapshotMerge returns the newest snapshot merge matching query.
func (supervisor *Supervisor) latestSnapshotMerge(ctx context.Context, query workspacecoord.MergeQuery) (workspacecoord.MergeRequest, error) {
	merges, err := supervisor.workspace.Repository.ListMerges(ctx, query)
	if err != nil {
		return workspacecoord.MergeRequest{}, fmt.Errorf("list snapshot merges: %w", err)
	}
	for _, merge := range slices.Backward(merges) {
		if merge.Kind == workspacecoord.MergeSnapshot {
			return merge, nil
		}
	}
	return workspacecoord.MergeRequest{}, nil
}

// latestMerge returns the newest merge of any kind matching query.
func (supervisor *Supervisor) latestMerge(ctx context.Context, query workspacecoord.MergeQuery) (workspacecoord.MergeRequest, error) {
	merges, err := supervisor.workspace.Repository.ListMerges(ctx, query)
	if err != nil {
		return workspacecoord.MergeRequest{}, fmt.Errorf("list merges: %w", err)
	}
	if len(merges) == 0 {
		return workspacecoord.MergeRequest{}, nil
	}
	return merges[len(merges)-1], nil
}

// artifactStore returns the coordinator's artifact store, which snapshot
// candidates are written to and read from.
func (supervisor *Supervisor) artifactStore() (workspacecoord.ArtifactStore, error) {
	store, ok := supervisor.workspace.Artifacts.(workspacecoord.ArtifactStore)
	if !ok {
		return nil, errors.New("snapshot candidates require a readable artifact store")
	}
	return store, nil
}

// openMergeStates lists the merge states that still hold undelivered work.
func openMergeStates() []workspacecoord.MergeState {
	return []workspacecoord.MergeState{workspacecoord.MergePending, workspacecoord.MergeRunning, workspacecoord.MergeConflicted}
}

func (supervisor *Supervisor) releaseWorkspaceView(ctx context.Context, agent orchestration.AgentSnapshot) error {
	if supervisor.workspace == nil {
		return nil
	}
	supervisor.mu.Lock()
	viewID := supervisor.workspaceViews[agent.ID]
	delete(supervisor.workspaceViews, agent.ID)
	supervisor.mu.Unlock()
	if viewID != "" {
		return supervisor.workspace.FinalizeView(ctx, viewID, "Agent reached terminal state")
	}
	views, err := supervisor.workspace.Repository.ListViews(ctx, workspacecoord.ViewQuery{
		TeamID: agent.TeamID, AgentID: agent.ID, States: []workspacecoord.ViewState{workspacecoord.ViewActive},
	})
	if err != nil {
		return err
	}
	for _, view := range views {
		if err := supervisor.workspace.FinalizeView(ctx, view.ID, "Agent reached terminal state"); err != nil {
			return err
		}
	}
	return nil
}

func (supervisor *Supervisor) releaseAgentLeases(ctx context.Context, agent orchestration.AgentSnapshot) error {
	if supervisor.leases == nil {
		return nil
	}
	leases, err := supervisor.leases.ListLeases(ctx, orchestration.LeaseQuery{TeamID: agent.TeamID, AgentID: agent.ID, States: []orchestration.LeaseState{orchestration.LeaseActive}})
	if err != nil {
		return err
	}
	for _, lease := range leases {
		if _, err := supervisor.leases.ReleaseLease(ctx, lease.ID, agent.ID, "Agent reached terminal state"); err != nil {
			return err
		}
	}
	return nil
}

func (supervisor *Supervisor) cleanupTerminalResources(ctx context.Context, agent orchestration.AgentSnapshot) error {
	return errors.Join(
		supervisor.releaseAgentLeases(ctx, agent),
		supervisor.releaseWorkspaceView(ctx, agent),
	)
}

// workspaceMode returns the granted workspace mode. Agents spawned before the
// mode was persisted derive it from their durable profile, as they did when
// they were created.
func (supervisor *Supervisor) workspaceMode(agent orchestration.AgentSnapshot) orchestration.WorkspaceMode {
	if agent.WorkspaceMode != "" {
		return agent.WorkspaceMode
	}
	profile, ok := supervisor.agentProfile(agent)
	return grantWorkspaceMode(agent.Workspace, profile, ok, orchestration.WorkspaceModeSharedLease)
}

// grantWorkspaceMode resolves a workspace request against the child's
// effective profile. A read-only request always wins, and read-only or git
// worktree profiles keep their mode. Every other writer, including children
// without profiles, gets the writer mode of the configured default.
func grantWorkspaceMode(request orchestration.WorkspaceRequest, profile agentprofile.Profile, profiled bool, writer orchestration.WorkspaceMode) orchestration.WorkspaceMode {
	if request == orchestration.WorkspaceReadonly {
		return orchestration.WorkspaceModeSharedReadonly
	}
	if !profiled {
		return writer
	}
	switch profile.WorkspaceMode {
	case agentprofile.WorkspaceSharedReadonly:
		return orchestration.WorkspaceModeSharedReadonly
	case agentprofile.WorkspaceGitWorktree:
		return orchestration.WorkspaceModeGitWorktree
	case agentprofile.WorkspaceSnapshot:
		return orchestration.WorkspaceModeSnapshot
	default:
		return writer
	}
}

// writerMode resolves the configured default to the mode granted to writers.
// Auto isolates writers in snapshots when the coordinator supports them.
func writerMode(choice WorkspaceDefault, workspace *workspacecoord.Coordinator) (orchestration.WorkspaceMode, error) {
	snapshots := workspace != nil && workspace.Snapshots != nil
	switch choice {
	case "", WorkspaceDefaultAuto:
		if snapshots {
			return orchestration.WorkspaceModeSnapshot, nil
		}
		return orchestration.WorkspaceModeSharedLease, nil
	case WorkspaceDefaultSnapshot:
		if !snapshots {
			return "", errors.New("snapshot workspace default requires a snapshot store")
		}
		return orchestration.WorkspaceModeSnapshot, nil
	case WorkspaceDefaultReadonly:
		return orchestration.WorkspaceModeSharedReadonly, nil
	case WorkspaceDefaultShared:
		return orchestration.WorkspaceModeSharedLease, nil
	default:
		return "", fmt.Errorf("unsupported default workspace %q", choice)
	}
}

// viewMode maps a granted workspace mode to the view that enforces it.
func viewMode(mode orchestration.WorkspaceMode) workspacecoord.ViewMode {
	switch mode {
	case orchestration.WorkspaceModeSharedReadonly:
		return workspacecoord.ViewSharedReadonly
	case orchestration.WorkspaceModeGitWorktree:
		return workspacecoord.ViewGitWorktree
	case orchestration.WorkspaceModeSnapshot:
		return workspacecoord.ViewSnapshot
	default:
		return workspacecoord.ViewSharedLease
	}
}

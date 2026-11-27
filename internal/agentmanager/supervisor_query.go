package agentmanager

import (
	"context"
	"errors"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

// ListAgents reads agent snapshots matching the query from the supervisor's repository.
func (supervisor *Supervisor) ListAgents(ctx context.Context, query orchestration.AgentQuery) ([]orchestration.AgentSnapshot, error) {
	if err := supervisor.ensureOpen(); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return supervisor.agents.ListAgents(ctx, query)
}

// GetTask reads the current task snapshot from the supervisor's configured repository.
func (supervisor *Supervisor) GetTask(ctx context.Context, taskID orchestration.TaskID) (orchestration.TaskSnapshot, error) {
	if err := supervisor.ensureOpen(); err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	if supervisor.tasks == nil {
		return orchestration.TaskSnapshot{}, errors.New("task repository is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return supervisor.tasks.GetTask(ctx, taskID)
}

// ListTasks reads task snapshots matching the query from the supervisor's task repository.
func (supervisor *Supervisor) ListTasks(ctx context.Context, query orchestration.TaskQuery) ([]orchestration.TaskSnapshot, error) {
	if err := supervisor.ensureOpen(); err != nil {
		return nil, err
	}
	if supervisor.tasks == nil {
		return nil, errors.New("task repository is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return supervisor.tasks.ListTasks(ctx, query)
}

// ListWorkspaceViews exposes the durable workspace projection without leaking
// the coordinator implementation into callers such as the TUI.
func (supervisor *Supervisor) ListWorkspaceViews(ctx context.Context, query workspacecoord.ViewQuery) ([]workspacecoord.View, error) {
	if err := supervisor.ensureOpen(); err != nil {
		return nil, err
	}
	if supervisor.workspace == nil || supervisor.workspace.Repository == nil {
		return nil, errors.New("workspace repository is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return supervisor.workspace.Repository.ListViews(ctx, query)
}

// ListWorkspaceLeases exposes active and historical path leases for activity
// projections. Lease mutations remain owned by the Supervisor and pipeline.
func (supervisor *Supervisor) ListWorkspaceLeases(ctx context.Context, query orchestration.LeaseQuery) ([]orchestration.WorkspaceLease, error) {
	if err := supervisor.ensureOpen(); err != nil {
		return nil, err
	}
	if supervisor.leases == nil {
		return nil, errors.New("workspace lease repository is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return supervisor.leases.ListLeases(ctx, query)
}

// ListMerges exposes the durable merge queue for read-only activity views.
func (supervisor *Supervisor) ListMerges(ctx context.Context, query workspacecoord.MergeQuery) ([]workspacecoord.MergeRequest, error) {
	if err := supervisor.ensureOpen(); err != nil {
		return nil, err
	}
	if supervisor.workspace == nil || supervisor.workspace.Repository == nil {
		return nil, errors.New("workspace repository is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return supervisor.workspace.Repository.ListMerges(ctx, query)
}

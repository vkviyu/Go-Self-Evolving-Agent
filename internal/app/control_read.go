package app

import (
	"context"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentmanager"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

func (a *App) ListAgents(ctx context.Context, query orchestration.AgentQuery) ([]orchestration.AgentSnapshot, error) {
	return a.repositories.ListAgents(ctx, query)
}

func (a *App) ListTasks(ctx context.Context, query orchestration.TaskQuery) ([]orchestration.TaskSnapshot, error) {
	return a.repositories.ListTasks(ctx, query)
}

func (a *App) ListWorkspaceViews(ctx context.Context, query workspacecoord.ViewQuery) ([]workspacecoord.View, error) {
	return a.repositories.ListViews(ctx, query)
}

func (a *App) ListMerges(ctx context.Context, query workspacecoord.MergeQuery) ([]workspacecoord.MergeRequest, error) {
	return a.repositories.ListMerges(ctx, query)
}

// LoadTeamSnapshot reads durable projections without creating a Supervisor.
// Workspace leases are runtime resources, not ATE business projections.
func (a *App) LoadTeamSnapshot(ctx context.Context, teamID orchestration.TeamID) (agentmanager.TeamSnapshot, error) {
	snapshot := agentmanager.TeamSnapshot{TeamID: teamID}
	if err := teamID.Validate(); err != nil {
		return snapshot, err
	}
	team, err := a.repositories.GetTeam(ctx, teamID)
	if err != nil {
		return snapshot, err
	}
	snapshot.State = team.State
	snapshot.Agents, err = a.ListAgents(ctx, orchestration.AgentQuery{TeamID: teamID})
	if err != nil {
		return snapshot, err
	}
	snapshot.Tasks, err = a.ListTasks(ctx, orchestration.TaskQuery{TeamID: teamID})
	if err != nil {
		return snapshot, err
	}
	snapshot.Views, err = a.ListWorkspaceViews(ctx, workspacecoord.ViewQuery{TeamID: teamID})
	if err != nil {
		return snapshot, err
	}
	snapshot.Merges, err = a.ListMerges(ctx, workspacecoord.MergeQuery{TeamID: teamID})
	if err != nil {
		return snapshot, err
	}
	// Keep empty arrays in the versioned snapshot protocol.
	if snapshot.Agents == nil {
		snapshot.Agents = []orchestration.AgentSnapshot{}
	}
	if snapshot.Tasks == nil {
		snapshot.Tasks = []orchestration.TaskSnapshot{}
	}
	if snapshot.Views == nil {
		snapshot.Views = []workspacecoord.View{}
	}
	if snapshot.Merges == nil {
		snapshot.Merges = []workspacecoord.MergeRequest{}
	}
	snapshot.Usage = make(map[orchestration.AgentID]tokenusage.UsageAggregate, len(snapshot.Agents))
	for _, agent := range snapshot.Agents {
		report, err := a.repositories.UsageReport(ctx, tokenusage.UsageQuery{TeamID: teamID, AgentID: agent.ID})
		if err != nil {
			return snapshot, err
		}
		snapshot.Usage[agent.ID] = report.Aggregate
	}
	return snapshot, nil
}

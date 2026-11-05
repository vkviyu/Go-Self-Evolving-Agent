package agentmanager

import (
	"context"
	"fmt"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

func (supervisor *Supervisor) proveReassignmentQuiescent(ctx context.Context, taskID orchestration.TaskID, failedID orchestration.AgentID) error {
	agent, err := supervisor.agents.GetAgent(ctx, failedID)
	if err != nil {
		return err
	}
	if !supervisor.teamAllowsExecution(ctx, agent.TeamID) {
		return orchestration.ErrTeamNotActive
	}
	agents, err := supervisor.agents.ListAgents(ctx, orchestration.AgentQuery{TeamID: agent.TeamID})
	if err != nil {
		return err
	}
	subtree := map[orchestration.AgentID]bool{failedID: true}
	for changed := true; changed; {
		changed = false
		for _, child := range agents {
			if subtree[child.ParentAgentID] && !subtree[child.ID] {
				subtree[child.ID] = true
				changed = true
			}
		}
	}
	for _, child := range agents {
		if !subtree[child.ID] {
			continue
		}
		if !isTerminal(child.State) {
			return fmt.Errorf("%w: descendant %s is still live", orchestration.ErrTaskReassignUnsafe, child.ID)
		}
		if supervisor.leases != nil {
			leases, err := supervisor.leases.ListLeases(ctx, orchestration.LeaseQuery{TeamID: agent.TeamID, AgentID: child.ID})
			if err != nil {
				return err
			}
			for _, lease := range leases {
				if lease.State != orchestration.LeaseReleased && lease.State != orchestration.LeaseAbandoned && lease.State != orchestration.LeaseRejected {
					return fmt.Errorf("%w: descendant %s has an unsettled workspace lease", orchestration.ErrTaskReassignUnsafe, child.ID)
				}
			}
		}
		if supervisor.workspace != nil {
			views, err := supervisor.workspace.Repository.ListViews(ctx, workspacecoord.ViewQuery{TeamID: agent.TeamID, AgentID: child.ID})
			if err != nil {
				return err
			}
			for _, view := range views {
				if view.State != workspacecoord.ViewReleased {
					return fmt.Errorf("%w: descendant %s has an active workspace view", orchestration.ErrTaskReassignUnsafe, child.ID)
				}
			}
		}
	}
	if supervisor.workspace != nil {
		merges, err := supervisor.workspace.Repository.ListMerges(ctx, workspacecoord.MergeQuery{TeamID: agent.TeamID})
		if err != nil {
			return err
		}
		for _, merge := range merges {
			if subtree[merge.AgentID] || merge.TaskID == taskID {
				return fmt.Errorf("%w: task attempt has workspace delivery history", orchestration.ErrTaskReassignUnsafe)
			}
		}
	}
	return nil
}

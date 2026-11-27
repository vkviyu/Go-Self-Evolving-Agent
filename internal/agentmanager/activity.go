package agentmanager

import (
	"context"
	"errors"
	"fmt"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

// TeamSnapshot is the read model shared by the TUI and headless control
// clients. It deliberately contains durable projections only; live worker
// maps and scheduler internals remain private to Supervisor.
type TeamSnapshot struct {
	TeamID orchestration.TeamID `json:"team_id"`
	// State is the Team lifecycle state when the repository records it.
	State  orchestration.TeamState        `json:"state,omitempty"`
	Agents []orchestration.AgentSnapshot  `json:"agents"`
	Tasks  []orchestration.TaskSnapshot   `json:"tasks"`
	Views  []workspacecoord.View          `json:"views"`
	Leases []orchestration.WorkspaceLease `json:"leases,omitempty"`
	Merges []workspacecoord.MergeRequest  `json:"merges"`
	// Usage holds each Agent's recorded token usage when the usage ledger
	// supports reports.
	Usage map[orchestration.AgentID]tokenusage.UsageAggregate `json:"usage,omitempty"`
}

// TeamSnapshotReader is the narrow read-only boundary used by presentation
// layers. Keeping this interface separate from Supervisor mutations makes
// adapters and tests independent from the lifecycle implementation.
type TeamSnapshotReader interface {
	// LoadTeamSnapshot reads the team's activity for display and control queries.
	LoadTeamSnapshot(context.Context, orchestration.TeamID) (TeamSnapshot, error)
}

// LoadTeamSnapshot reads all durable activity projections for one team. A
// partial snapshot is returned together with joined errors so a degraded
// optional projection does not hide the rest of the activity view.
func (supervisor *Supervisor) LoadTeamSnapshot(ctx context.Context, teamID orchestration.TeamID) (TeamSnapshot, error) {
	snapshot := TeamSnapshot{TeamID: teamID}
	if err := supervisor.ensureOpen(); err != nil {
		return snapshot, err
	}
	if err := teamID.Validate(); err != nil {
		return snapshot, fmt.Errorf("team snapshot: %w", err)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var errs []error
	load := func(name string, read func() error) {
		if err := read(); err != nil {
			errs = append(errs, fmt.Errorf("%s projection: %w", name, err))
		}
	}
	if teams, ok := supervisor.agents.(orchestration.TeamRepository); ok {
		load("team", func() error {
			team, err := teams.GetTeam(ctx, teamID)
			snapshot.State = team.State
			return err
		})
	}
	load("agent", func() error {
		values, err := supervisor.ListAgents(ctx, orchestration.AgentQuery{TeamID: teamID})
		supervisor.overlayActivity(values)
		snapshot.Agents = values
		return err
	})
	load("task", func() error {
		values, err := supervisor.ListTasks(ctx, orchestration.TaskQuery{TeamID: teamID})
		snapshot.Tasks = values
		return err
	})
	load("workspace view", func() error {
		values, err := supervisor.ListWorkspaceViews(ctx, workspacecoord.ViewQuery{TeamID: teamID})
		snapshot.Views = values
		return err
	})
	load("merge", func() error {
		values, err := supervisor.ListMerges(ctx, workspacecoord.MergeQuery{TeamID: teamID})
		snapshot.Merges = values
		return err
	})
	if supervisor.leases != nil {
		load("workspace lease", func() error {
			values, err := supervisor.ListWorkspaceLeases(ctx, orchestration.LeaseQuery{TeamID: teamID})
			snapshot.Leases = values
			return err
		})
	}
	if reader, ok := supervisor.usageLedger.(tokenusage.Reader); ok {
		load("usage", func() error {
			snapshot.Usage = make(map[orchestration.AgentID]tokenusage.UsageAggregate, len(snapshot.Agents))
			for _, agent := range snapshot.Agents {
				report, err := reader.UsageReport(ctx, tokenusage.UsageQuery{TeamID: teamID, AgentID: agent.ID})
				if err != nil {
					return err
				}
				snapshot.Usage[agent.ID] = report.Aggregate
			}
			return nil
		})
	}
	return snapshot, errors.Join(errs...)
}

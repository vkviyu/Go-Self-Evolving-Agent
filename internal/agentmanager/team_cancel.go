package agentmanager

import (
	"context"
	"errors"
	"fmt"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

// CancelTeam stops every child Agent of a Team and marks the Team canceled.
// The Team leaves active first, so no child is admitted while the running
// ones are interrupted. The root Agent belongs to its session and keeps
// running; reopening the session makes the Team active again. ctx bounds the
// wait for interrupted runners, and repeating the call finishes a cancel that
// stopped partway.
func (supervisor *Supervisor) CancelTeam(ctx context.Context, teamID orchestration.TeamID, reason string) (orchestration.TeamSnapshot, error) {
	if err := supervisor.ensureOpen(); err != nil {
		return orchestration.TeamSnapshot{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	teams, ok := supervisor.agents.(orchestration.TeamRepository)
	if !ok {
		return orchestration.TeamSnapshot{}, errors.New("Agent repository does not support Team transitions")
	}
	if reason == "" {
		reason = "Team cancel requested"
	}
	team, err := supervisor.beginTeamCancel(ctx, teams, teamID, reason)
	if err != nil || team.State == orchestration.TeamCanceled {
		return team, err
	}
	supervisor.rememberTeam(teamID)
	agents, err := supervisor.agents.ListAgents(ctx, orchestration.AgentQuery{TeamID: teamID, States: activeAgentStates()})
	if err != nil {
		return team, fmt.Errorf("list Agents of Team %s: %w", teamID, err)
	}
	var children []orchestration.AgentID
	var errs []error
	for _, agent := range agents {
		if agent.ParentAgentID == "" {
			continue
		}
		children = append(children, agent.ID)
		// Interrupt also stops descendants; those already stopped report
		// ErrAgentTerminal.
		if err := supervisor.Interrupt(ctx, agent.ID); err != nil && !errors.Is(err, ErrAgentTerminal) {
			errs = append(errs, fmt.Errorf("interrupt Agent %s: %w", agent.ID, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return team, err
	}
	if len(children) > 0 {
		// A running child becomes canceled when its runner returns.
		if _, err := supervisor.Wait(ctx, WaitCommand{AgentIDs: children}); err != nil {
			return team, fmt.Errorf("Team %s is still canceling: %w", teamID, err)
		}
	}
	return teams.TransitionTeam(ctx, orchestration.TeamTransition{TeamID: teamID, Expected: team.Version, Target: orchestration.TeamCanceled, Reason: reason})
}

// beginTeamCancel moves an active or draining Team to canceling. Admission is
// held so no in-process spawn passes its checks against the active Team.
func (supervisor *Supervisor) beginTeamCancel(ctx context.Context, teams orchestration.TeamRepository, teamID orchestration.TeamID, reason string) (orchestration.TeamSnapshot, error) {
	supervisor.admission.Lock()
	defer supervisor.admission.Unlock()
	team, err := teams.GetTeam(ctx, teamID)
	if err != nil || team.State == orchestration.TeamCanceling || team.State == orchestration.TeamCanceled {
		return team, err
	}
	return teams.TransitionTeam(ctx, orchestration.TeamTransition{TeamID: teamID, Expected: team.Version, Target: orchestration.TeamCanceling, Reason: reason})
}

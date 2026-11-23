package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

// Validate before opening any runtime resources, including for unknown commands.
func validateControlCommand(command controlCommand) (bool, error) {
	if command.TeamID != "" {
		if err := orchestration.TeamID(command.TeamID).Validate(); err != nil {
			return false, err
		}
	}
	required := func(value, name string) (bool, error) {
		if value == "" {
			return false, fmt.Errorf("%s is required", name)
		}
		return false, nil
	}
	switch command.Command {
	case "snapshot", "agent.list", "task.list", "workspace.list", "merge.list", "events":
		return true, nil
	case "agent.cancel", "wait":
		return required(command.AgentID, "agent_id")
	case "team.cancel":
		return required(command.TeamID, "team_id")
	case "merge.run":
		if command.TeamID == "" && command.AgentID == "" && command.TaskID == "" && command.MergeID == "" {
			return false, errors.New("merge.run requires a Team or target ID")
		}
		return false, nil
	case "merge.requeue":
		if command.MergeID == "" {
			return false, errors.New("merge_id is required")
		}
		return required(command.ResolutionTaskID, "resolution_task_id")
	case "task.transition":
		if command.TaskID == "" {
			return false, errors.New("task_id is required")
		}
		return required(command.ActorAgentID, "actor_agent_id")
	case "":
		return false, errors.New("command is required")
	default:
		return false, fmt.Errorf("unknown control command %q", command.Command)
	}
}

type controlTargetReader interface {
	GetTeam(context.Context, orchestration.TeamID) (orchestration.TeamSnapshot, error)
	GetAgent(context.Context, orchestration.AgentID) (orchestration.AgentSnapshot, error)
	GetTask(context.Context, orchestration.TaskID) (orchestration.TaskSnapshot, error)
	GetMerge(context.Context, orchestration.MergeID) (workspacecoord.MergeRequest, error)
}

// Every supplied identifier is checked, even if it is not the primary target.
// In particular an actor or resolution task cannot authorize another Team.
func resolveControlTeam(ctx context.Context, store controlTargetReader, command controlCommand) (orchestration.TeamID, error) {
	teamID := orchestration.TeamID(command.TeamID)
	accept := func(id orchestration.TeamID) error {
		if err := id.Validate(); err != nil {
			return err
		}
		if teamID != "" && teamID != id {
			return errors.New("control identifiers belong to different Teams")
		}
		teamID = id
		return nil
	}
	for _, value := range []string{command.AgentID, command.ActorAgentID} {
		if value == "" {
			continue
		}
		id := orchestration.AgentID(value)
		if err := id.Validate(); err != nil {
			return "", err
		}
		agent, err := store.GetAgent(ctx, id)
		if err != nil {
			return "", err
		}
		if err := accept(agent.TeamID); err != nil {
			return "", err
		}
	}
	for _, value := range []string{command.TaskID, command.ResolutionTaskID} {
		if value == "" {
			continue
		}
		id := orchestration.TaskID(value)
		if err := id.Validate(); err != nil {
			return "", err
		}
		task, err := store.GetTask(ctx, id)
		if err != nil {
			return "", err
		}
		if err := accept(task.TeamID); err != nil {
			return "", err
		}
	}
	if command.MergeID != "" {
		id := orchestration.MergeID(command.MergeID)
		if err := id.Validate(); err != nil {
			return "", err
		}
		merge, err := store.GetMerge(ctx, id)
		if err != nil {
			return "", err
		}
		if err := accept(merge.TeamID); err != nil {
			return "", err
		}
	}
	if err := teamID.Validate(); err != nil {
		return "", err
	}
	team, err := store.GetTeam(ctx, teamID)
	if err != nil {
		return "", err
	}
	if team.ID != teamID {
		return "", errors.New("control Team identity mismatch")
	}
	return teamID, nil
}

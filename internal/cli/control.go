package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentmanager"
	"github.com/vkviyu/go-self-evolving-agent/internal/app"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

const controlProtocol = "gsea.control.v1"

type controlCommand struct {
	Protocol         string   `json:"protocol"`
	Command          string   `json:"command"`
	TeamID           string   `json:"team_id"`
	AgentID          string   `json:"agent_id"`
	ActorAgentID     string   `json:"actor_agent_id"`
	TaskID           string   `json:"task_id"`
	MergeID          string   `json:"merge_id"`
	ResolutionTaskID string   `json:"resolution_task_id"`
	Target           string   `json:"target"`
	ExpectedVersion  *uint64  `json:"expected_version"`
	Reason           string   `json:"reason"`
	States           []string `json:"states"`
	Limit            int      `json:"limit"`
	After            uint64   `json:"after"`
	TimeoutMS        int      `json:"timeout_ms"`
}

func runControl(ctx context.Context, arguments []string, stdin io.Reader, stdout, stderr io.Writer) (resultErr error) {
	flags := flag.NewFlagSet("control", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dataDirectory := flags.String("data-dir", "", "override the local Agent data directory")
	workspaceDirectory := flags.String("workspace", "", "override the workspace directory")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("control does not accept positional arguments")
	}
	resolved, err := config.Load(config.LoadOptions{})
	if err != nil {
		return err
	}
	if *dataDirectory != "" {
		resolved.DataDirectory = *dataDirectory
	}
	if *workspaceDirectory != "" {
		resolved.WorkspaceDirectory = *workspaceDirectory
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var command controlCommand
		if err := json.Unmarshal([]byte(line), &command); err != nil {
			if writeErr := writeControl(encoder, map[string]any{"protocol": controlProtocol, "type": "error", "error": "invalid JSON: " + err.Error()}); writeErr != nil {
				return writeErr
			}
			continue
		}
		if command.Protocol != "" && command.Protocol != "gsea.command.v1" {
			if err := writeControl(encoder, map[string]any{"protocol": controlProtocol, "type": "error", "error": "unsupported command protocol"}); err != nil {
				return err
			}
			continue
		}
		if err := runControlCommand(ctx, resolved, command, encoder); err != nil {
			if writeErr := writeControl(encoder, map[string]any{"protocol": controlProtocol, "type": "error", "command": command.Command, "error": err.Error()}); writeErr != nil {
				return writeErr
			}
		}
	}
	return scanner.Err()
}

type controlProjection interface {
	ListTeams(context.Context) ([]orchestration.TeamID, error)
	LoadGlobal(context.Context, orchestration.Cursor, int) ([]orchestration.Event, error)
	LoadTeam(context.Context, orchestration.TeamID, orchestration.Cursor, int) ([]orchestration.Event, error)
}

type controlReader interface {
	agentmanager.TeamSnapshotReader
	ListAgents(context.Context, orchestration.AgentQuery) ([]orchestration.AgentSnapshot, error)
	ListTasks(context.Context, orchestration.TaskQuery) ([]orchestration.TaskSnapshot, error)
	ListWorkspaceViews(context.Context, workspacecoord.ViewQuery) ([]workspacecoord.View, error)
	ListMerges(context.Context, workspacecoord.MergeQuery) ([]workspacecoord.MergeRequest, error)
}

func runControlCommand(ctx context.Context, cfg config.Config, command controlCommand, encoder *json.Encoder) (resultErr error) {
	readOnly, err := validateControlCommand(command)
	if err != nil {
		return err
	}
	application, err := app.OpenControl(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, application.Close()) }()
	if readOnly {
		return executeControl(ctx, application, nil, application.Orchestration(), command, encoder)
	}
	teamID, err := resolveControlTeam(ctx, application.Orchestration(), command)
	if err != nil {
		return err
	}
	if err := application.AttachExistingTeam(ctx, teamID); err != nil {
		return err
	}
	// Revalidate under ownership, before Supervisor recovery can write.
	if resolved, err := resolveControlTeam(ctx, application.Orchestration(), command); err != nil {
		return err
	} else if resolved != teamID {
		return errors.New("control target Team changed while acquiring ownership")
	}
	supervisor, err := application.NewControlSupervisor(ctx, teamID)
	if err != nil {
		return err
	}
	command.TeamID = string(teamID)
	return executeControlCommand(ctx, supervisor, application.Orchestration(), command, encoder)
}

func executeControlCommand(ctx context.Context, supervisor *agentmanager.Supervisor, store controlProjection, command controlCommand, encoder *json.Encoder) error {
	return executeControl(ctx, supervisor, supervisor, store, command, encoder)
}

func executeControl(ctx context.Context, reader controlReader, supervisor *agentmanager.Supervisor, store controlProjection, command controlCommand, encoder *json.Encoder) error {
	if command.Command == "" {
		return errors.New("command is required")
	}
	teamID := orchestration.TeamID(command.TeamID)
	if teamID != "" {
		if err := teamID.Validate(); err != nil {
			return err
		}
	}
	queryAgents := func() ([]orchestration.AgentSnapshot, error) {
		return reader.ListAgents(ctx, orchestration.AgentQuery{TeamID: teamID})
	}
	queryTasks := func() ([]orchestration.TaskSnapshot, error) {
		return reader.ListTasks(ctx, orchestration.TaskQuery{TeamID: teamID})
	}
	queryViews := func() ([]workspacecoord.View, error) {
		return reader.ListWorkspaceViews(ctx, workspacecoord.ViewQuery{TeamID: teamID})
	}
	queryMerges := func() ([]workspacecoord.MergeRequest, error) {
		return reader.ListMerges(ctx, workspacecoord.MergeQuery{TeamID: teamID})
	}
	switch command.Command {
	case "snapshot":
		if teamID == "" {
			teams, err := store.ListTeams(ctx)
			if err != nil {
				return err
			}
			for _, id := range teams {
				if err := emitSnapshot(ctx, reader, id, encoder); err != nil {
					return err
				}
			}
			return nil
		}
		return emitSnapshot(ctx, reader, teamID, encoder)
	case "agent.list":
		agents, err := queryAgents()
		return emitResult(encoder, command.Command, teamID, agents, err)
	case "task.list":
		tasks, err := queryTasks()
		return emitResult(encoder, command.Command, teamID, tasks, err)
	case "workspace.list":
		views, err := queryViews()
		return emitResult(encoder, command.Command, teamID, views, err)
	case "merge.list":
		merges, err := queryMerges()
		return emitResult(encoder, command.Command, teamID, merges, err)
	case "events":
		var events []orchestration.Event
		var err error
		if teamID == "" {
			events, err = store.LoadGlobal(ctx, orchestration.Cursor(command.After), command.Limit)
		} else {
			events, err = store.LoadTeam(ctx, teamID, orchestration.Cursor(command.After), command.Limit)
		}
		if err != nil {
			return err
		}
		next := command.After
		for _, event := range events {
			if err := writeControl(encoder, map[string]any{"protocol": controlProtocol, "type": string(event.Type), "cursor": event.GlobalSequence, "team_id": event.TeamID, "event_id": event.EventID, "agent_id": event.ActorAgentID, "payload": json.RawMessage(event.Payload)}); err != nil {
				return err
			}
			next = event.GlobalSequence
		}
		// The page result carries the cursor of the next request; an empty
		// page keeps the request cursor.
		return emitResult(encoder, command.Command, teamID, map[string]any{"count": len(events), "next_cursor": next})
	case "agent.cancel":
		if err := supervisor.Interrupt(ctx, orchestration.AgentID(command.AgentID)); err != nil {
			return err
		}
		return emitResult(encoder, command.Command, teamID, map[string]any{"agent_id": command.AgentID, "state": orchestration.AgentCanceling})
	case "team.cancel":
		if teamID == "" {
			return errors.New("team_id is required")
		}
		// timeout_ms bounds the wait for interrupted runners; a timed-out
		// cancel leaves the Team canceling until it is repeated.
		cancelCtx := ctx
		if command.TimeoutMS > 0 {
			var cancel context.CancelFunc
			cancelCtx, cancel = context.WithTimeout(ctx, time.Duration(command.TimeoutMS)*time.Millisecond)
			defer cancel()
		}
		team, err := supervisor.CancelTeam(cancelCtx, teamID, command.Reason)
		return emitResult(encoder, command.Command, teamID, team, err)
	case "merge.run":
		merges, err := supervisor.RunPendingMerges(ctx, teamID, command.Limit)
		return emitResult(encoder, command.Command, teamID, merges, err)
	case "merge.requeue":
		merge, err := supervisor.RequeueResolvedMerge(ctx, orchestration.MergeID(command.MergeID), orchestration.TaskID(command.ResolutionTaskID))
		return emitResult(encoder, command.Command, teamID, merge, err)
	case "task.transition":
		if command.ActorAgentID == "" {
			return errors.New("actor_agent_id is required")
		}
		task, err := supervisor.TransitionTask(ctx, orchestration.TaskTransition{TaskID: orchestration.TaskID(command.TaskID), Expected: valueOrZero(command.ExpectedVersion), Target: orchestration.TaskState(command.Target), ActorAgentID: orchestration.AgentID(command.ActorAgentID), Reason: command.Reason})
		return emitResult(encoder, command.Command, teamID, task, err)
	case "wait":
		if command.AgentID == "" {
			return errors.New("agent_id is required")
		}
		timeout := time.Duration(command.TimeoutMS) * time.Millisecond
		result, err := supervisor.Wait(ctx, agentmanager.WaitCommand{AgentIDs: []orchestration.AgentID{orchestration.AgentID(command.AgentID)}, Timeout: timeout})
		return emitResult(encoder, command.Command, teamID, result, err)
	default:
		return fmt.Errorf("unknown control command %q", command.Command)
	}
}

type controlSnapshot struct {
	Protocol string `json:"protocol"`
	Type     string `json:"type"`
	agentmanager.TeamSnapshot
}

func emitSnapshot(ctx context.Context, reader agentmanager.TeamSnapshotReader, teamID orchestration.TeamID, encoder *json.Encoder) error {
	snapshot, err := reader.LoadTeamSnapshot(ctx, teamID)
	if err != nil {
		return err
	}
	return writeControl(encoder, controlSnapshot{Protocol: controlProtocol, Type: "snapshot", TeamSnapshot: snapshot})
}

func emitResult(encoder *json.Encoder, command string, teamID orchestration.TeamID, value any, values ...error) error {
	for _, err := range values {
		if err != nil {
			return err
		}
	}
	return writeControl(encoder, map[string]any{"protocol": controlProtocol, "type": "result", "command": command, "team_id": teamID, "data": value})
}

func valueOrZero(value *uint64) uint64 {
	if value == nil {
		return orchestration.AnySequence
	}
	return *value
}

func writeControl(encoder *json.Encoder, value any) error {
	return encoder.Encode(value)
}

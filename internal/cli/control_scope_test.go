package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/app"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

type controlTargets struct{}

const (
	controlTeamA        = "team_00000000000070008000000000000001"
	controlTeamB        = "team_00000000000070008000000000000002"
	controlAgent        = "agent_00000000000070008000000000000001"
	controlForeignAgent = "agent_00000000000070008000000000000002"
	controlMissingAgent = "agent_00000000000070008000000000000003"
	controlTask         = "task_00000000000070008000000000000001"
	controlForeignTask  = "task_00000000000070008000000000000002"
	controlMerge        = "merge_00000000000070008000000000000001"
	controlForeignMerge = "merge_00000000000070008000000000000002"
)

func (controlTargets) GetTeam(_ context.Context, id orchestration.TeamID) (orchestration.TeamSnapshot, error) {
	return orchestration.TeamSnapshot{ID: id}, nil
}
func (controlTargets) GetAgent(_ context.Context, id orchestration.AgentID) (orchestration.AgentSnapshot, error) {
	if id == controlMissingAgent {
		return orchestration.AgentSnapshot{}, orchestration.ErrNotFound
	}
	team := orchestration.TeamID(controlTeamA)
	if id == controlForeignAgent {
		team = controlTeamB
	}
	return orchestration.AgentSnapshot{ID: id, TeamID: team}, nil
}
func (controlTargets) GetTask(_ context.Context, id orchestration.TaskID) (orchestration.TaskSnapshot, error) {
	team := orchestration.TeamID(controlTeamA)
	if id == controlForeignTask {
		team = controlTeamB
	}
	return orchestration.TaskSnapshot{ID: id, TeamID: team}, nil
}
func (controlTargets) GetMerge(_ context.Context, id orchestration.MergeID) (workspacecoord.MergeRequest, error) {
	team := orchestration.TeamID(controlTeamA)
	if id == controlForeignMerge {
		team = controlTeamB
	}
	return workspacecoord.MergeRequest{ID: id, TeamID: team}, nil
}

func TestControlScopeTargetResolution(t *testing.T) {
	for _, command := range []controlCommand{
		{Command: "agent.cancel", AgentID: controlAgent},
		{Command: "wait", AgentID: controlAgent},
		{Command: "task.transition", TaskID: controlTask, ActorAgentID: controlAgent},
		{Command: "merge.requeue", MergeID: controlMerge, ResolutionTaskID: controlTask},
		{Command: "merge.run", TaskID: controlTask},
		{Command: "team.cancel", TeamID: controlTeamA},
	} {
		t.Run(command.Command, func(t *testing.T) {
			if readOnly, err := validateControlCommand(command); err != nil || readOnly {
				t.Fatalf("validate mutation: %t, %v", readOnly, err)
			}
			if team, err := resolveControlTeam(t.Context(), controlTargets{}, command); err != nil || team != controlTeamA {
				t.Fatalf("resolve: %s, %v", team, err)
			}
		})
	}
	for _, command := range []controlCommand{
		{TeamID: controlTeamB, AgentID: controlAgent},
		{TaskID: controlTask, ActorAgentID: controlForeignAgent},
		{MergeID: controlMerge, ResolutionTaskID: controlForeignTask},
		{TaskID: controlTask, MergeID: controlForeignMerge},
		{TeamID: controlTeamA, AgentID: controlForeignAgent},
		{},
	} {
		if team, err := resolveControlTeam(t.Context(), controlTargets{}, command); err == nil {
			t.Fatalf("accepted inconsistent/unscoped command %+v as %s", command, team)
		}
	}
	if _, err := resolveControlTeam(t.Context(), controlTargets{}, controlCommand{AgentID: controlMissingAgent}); !errors.Is(err, orchestration.ErrNotFound) {
		t.Fatalf("missing target: %v", err)
	}
	for _, name := range []string{"snapshot", "events", "agent.list", "task.list", "workspace.list", "merge.list"} {
		if readOnly, err := validateControlCommand(controlCommand{Command: name}); err != nil || !readOnly {
			t.Fatalf("read command %s requires execution: %v", name, err)
		}
	}
	for _, command := range []controlCommand{
		{Command: "merge.run"}, {Command: "unknown"}, {Command: "team.cancel"},
		{Command: "agent.cancel"}, {Command: "task.transition", TaskID: "task"},
		{Command: "merge.requeue", MergeID: "merge"},
	} {
		if _, err := validateControlCommand(command); err == nil {
			t.Fatalf("accepted invalid command: %+v", command)
		}
	}
}

func TestControlScopeProductionJSONL(t *testing.T) {
	ctx := t.Context()
	binary := cliATE.binary
	home, data, workspace := t.TempDir(), t.TempDir(), t.TempDir()
	directory := filepath.Join(t.TempDir(), "ate")
	t.Setenv(config.HomeEnvironment, home)
	t.Setenv(config.ProfileEnvironment, "")
	t.Setenv("GSEA_ATE_ENABLED", "true")
	t.Setenv("GSEA_ATE_BINARY", binary)
	t.Setenv("GSEA_ATE_DIRECTORY", directory)
	t.Setenv("GSEA_ATE_TIMEOUT", "30s")
	cfg := config.Config{
		HomeDirectory: home, DataDirectory: data, WorkspaceDirectory: workspace,
		ATE: config.ATEConfig{Enabled: true, Binary: binary, Directory: directory, Timeout: 30 * time.Second},
	}
	seed, err := app.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = seed.Close() })
	a, _, err := seed.EnsureRootThread(ctx, session.ID("jsonl-a"))
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := seed.EnsureRootThread(ctx, session.ID("jsonl-b"))
	if err != nil {
		t.Fatal(err)
	}
	task, err := seed.Orchestration().CreateTask(ctx, orchestration.TaskSpec{TeamID: a.TeamID, ActorAgentID: a.AgentID, Title: "ready task", IdempotencyKey: "task"})
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := seed.Orchestration().CreateTask(ctx, orchestration.TaskSpec{TeamID: a.TeamID, ActorAgentID: a.AgentID, Title: "dependent task", IdempotencyKey: "blocked"})
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Orchestration().AddDependency(ctx, blocked.ID, task.ID, a.AgentID, "dependency"); err != nil {
		t.Fatal(err)
	}
	// Queries work even while chat owns the runtime database and both Sessions.
	for _, name := range []string{"snapshot", "agent.list", "task.list", "workspace.list", "merge.list", "events"} {
		var output, diagnostic bytes.Buffer
		input := strings.NewReader(fmt.Sprintf("{\"command\":%q,\"team_id\":%q}\n", name, a.TeamID))
		if err := runControl(ctx, []string{"--data-dir", data, "--workspace", workspace}, input, &output, &diagnostic); err != nil {
			t.Fatalf("%s while chat owns Sessions: %v", name, err)
		}
		if output.Len() == 0 || bytes.Contains(output.Bytes(), []byte(`"type":"error"`)) {
			t.Fatalf("%s while chat owns Sessions: %s", name, output.String())
		}
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	// Both mutating commands omit team_id where a target ID is sufficient.
	// A failed cross-Team request must not prevent later JSONL commands.
	input := fmt.Sprintf(`{"command":"snapshot"}
{"command":"task.transition","task_id":%q,"actor_agent_id":%q,"target":"ready"}
{"command":"task.transition","task_id":%q,"actor_agent_id":%q,"target":"ready"}
{"command":"task.transition","task_id":%q,"actor_agent_id":%q,"target":"ready"}
{"command":"team.cancel","team_id":%q}
{"command":"team.cancel","team_id":%q}
{"command":"snapshot"}
`, task.ID, b.AgentID, task.ID, a.AgentID, blocked.ID, a.AgentID, a.TeamID, b.TeamID)
	var stdout, stderr bytes.Buffer
	if err := runControl(ctx, []string{"--data-dir", data, "--workspace", workspace}, strings.NewReader(input), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&stdout)
	var responses []struct {
		Protocol string `json:"protocol"`
		Type     string `json:"type"`
		Command  string `json:"command"`
		TeamID   string `json:"team_id"`
		State    string `json:"state"`
		Error    string `json:"error"`
	}
	for decoder.More() {
		var response struct {
			Protocol string `json:"protocol"`
			Type     string `json:"type"`
			Command  string `json:"command"`
			TeamID   string `json:"team_id"`
			State    string `json:"state"`
			Error    string `json:"error"`
		}
		if err := decoder.Decode(&response); err != nil {
			t.Fatal(err)
		}
		if response.Protocol != controlProtocol {
			t.Fatalf("protocol changed: %+v", response)
		}
		responses = append(responses, response)
	}
	if len(responses) != 9 {
		t.Fatalf("responses: %+v", responses)
	}
	if responses[2].Type != "error" || !strings.Contains(responses[2].Error, "different Teams") {
		t.Fatalf("cross-Team mutation accepted: %+v", responses[2])
	}
	if responses[4].Type != "error" || !strings.Contains(responses[4].Error, orchestration.ErrTaskNotReady.Error()) {
		t.Fatalf("dependency validation bypassed: %+v", responses[4])
	}
	for _, index := range []int{3, 5, 6} {
		if responses[index].Type != "result" {
			t.Fatalf("mutation failed: %+v", responses[index])
		}
	}
	if responses[3].TeamID != string(a.TeamID) {
		t.Fatalf("task Team not resolved: %+v", responses[3])
	}
	for _, index := range []int{7, 8} {
		if responses[index].Type != "snapshot" || responses[index].State != "canceled" {
			t.Fatalf("snapshot reopened canceled Team: %+v", responses[index])
		}
	}
}

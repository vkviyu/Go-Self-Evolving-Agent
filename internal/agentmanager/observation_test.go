package agentmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/observation"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/testfixture"
)

func TestParallelRunsKeepParentAndIndependentObservationScopes(t *testing.T) {
	rec, err := observation.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rec.Close(context.Background()) })
	ctx, cancel := context.WithTimeout(rec.Attach(t.Context()), 15*time.Second)
	defer cancel()
	db, err := testfixture.Open(t, ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := db.Orchestration()
	root, _, err := repo.EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "root"})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan observation.Scope, 2)
	release := make(chan struct{})
	runner := RunnerFunc(func(ctx context.Context, _ RunRequest) (orchestration.AgentResult, error) {
		started <- observation.ScopeFrom(ctx)
		select {
		case <-release:
			return orchestration.AgentResult{Status: orchestration.ResultSucceeded}, nil
		case <-ctx.Done():
			return orchestration.AgentResult{}, ctx.Err()
		}
	})
	s, err := NewSupervisor(Config{RunContext: observation.WithScope(ctx, observation.Scope{SessionID: "old-session", TurnID: "old-turn", CallID: "old-call", RequestID: "old-request", TaskID: "old-task"}), Agents: repo, Mailbox: repo, Leases: newLeaseStore(t), Runner: runner, MaxRunningAgents: 2, MaxLiveAgents: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	caller := observation.WithScope(ctx, observation.Scope{SessionID: "root", TeamID: string(root.TeamID), AgentID: string(root.AgentID), TurnID: "parent-turn", CallID: "parent-call"})
	children := map[string]orchestration.AgentSnapshot{}
	for _, key := range []string{"first", "second"} {
		spec := childSpec(root, key, "private prompt")
		spec.Workspace = orchestration.WorkspaceReadonly
		child, err := s.Spawn(caller, spec)
		if err != nil {
			t.Fatal(err)
		}
		children[string(child.ID)] = child
	}
	runs := map[string]string{}
	for range 2 {
		select {
		case worker := <-started:
			child, ok := children[worker.AgentID]
			if !ok || worker.RunID == "" || worker.TeamID != string(root.TeamID) || worker.ParentAgentID != string(root.AgentID) || worker.SessionID != string(child.SessionID) || worker.TurnID != "" || worker.CallID != "" || worker.RequestID != "" || worker.TaskID != "" {
				t.Fatalf("wrong child scope in runner: %+v", worker)
			}
			runs[worker.AgentID] = worker.RunID
		case <-ctx.Done():
			t.Fatal("both independent workers must reach the barrier")
		}
	}
	close(release)
	for _, child := range children {
		res, err := s.Wait(ctx, WaitCommand{AgentIDs: []orchestration.AgentID{child.ID}, Timeout: 3 * time.Second})
		if err != nil || !res.Satisfied {
			t.Fatalf("worker wait: %+v %v", res, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rec.Close(ctx); err != nil {
		t.Fatal(err)
	}
	trace, err := os.ReadFile(rec.Path())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(trace, []byte("private prompt")) {
		t.Fatal("recorded child input")
	}
	spawned := map[string]int{}
	finished := map[string]int{}
	for _, line := range bytes.Split(bytes.TrimSpace(trace), []byte{'\n'}) {
		var e observation.Event
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatal(err)
		}
		switch e.Name {
		case "agent.spawned":
			if e.TurnID != "parent-turn" || e.CallID != "parent-call" || e.AgentID != string(root.AgentID) {
				t.Fatalf("broken spawn edge: %+v", e)
			}
			spawned[e.Labels["child_agent_id"]]++
		case "agent.run.finished":
			if e.ParentAgentID != string(root.AgentID) || runs[e.AgentID] != e.RunID || e.Status != string(orchestration.AgentCompleted) {
				t.Fatalf("broken child result: %+v", e)
			}
			finished[e.AgentID]++
		}
	}
	for id := range children {
		if spawned[id] != 1 || finished[id] != 1 {
			t.Errorf("child %s: spawn=%d finish=%d", id, spawned[id], finished[id])
		}
	}
}

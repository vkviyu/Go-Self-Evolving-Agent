package agentmanager

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

func TestCancelTeamStopsChildrenAndRejectsSpawns(t *testing.T) {
	ctx := context.Background()
	runner := &testRunner{started: make(chan RunRequest, 2)}
	supervisor, store, root := newSupervisorTestWithRunning(t, runner, 1, 4, 3, 4)
	running, err := supervisor.Spawn(ctx, childSpec(root, "team-running", "wait for cancellation"))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("child did not start")
	}
	// With one run slot the second child stays queued.
	queued, err := supervisor.Spawn(ctx, childSpec(root, "team-queued", "never run"))
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	team, err := supervisor.CancelTeam(waitCtx, root.TeamID, "")
	if err != nil || team.State != orchestration.TeamCanceled {
		t.Fatalf("team = %#v, err=%v", team, err)
	}
	for _, id := range []orchestration.AgentID{running.ID, queued.ID} {
		persisted, err := store.GetAgent(ctx, id)
		if err != nil || persisted.State != orchestration.AgentCanceled {
			t.Fatalf("Agent %s = %s, want canceled; err=%v", id, persisted.State, err)
		}
	}
	// The root keeps its session; only new children are rejected.
	if persisted, err := store.GetAgent(ctx, root.AgentID); err != nil || isTerminal(persisted.State) {
		t.Fatalf("root = %s, err=%v", persisted.State, err)
	}
	if _, err := supervisor.Spawn(ctx, childSpec(root, "team-late", "late")); !errors.Is(err, orchestration.ErrTeamNotActive) {
		t.Fatalf("spawn after cancel error = %v", err)
	}
	// Repeating the cancel is a no-op on the canceled Team.
	if again, err := supervisor.CancelTeam(ctx, root.TeamID, ""); err != nil || again.Version != team.Version {
		t.Fatalf("repeated cancel = %#v, err=%v", again, err)
	}
	// The fixture has no task or workspace repositories, so only the Team
	// projection is checked.
	snapshot, _ := supervisor.LoadTeamSnapshot(ctx, root.TeamID)
	if snapshot.State != orchestration.TeamCanceled {
		t.Fatalf("snapshot state = %q", snapshot.State)
	}
}

func TestRecoveryDoesNotRunQueuedChildrenOfCancelingTeam(t *testing.T) {
	ctx := context.Background()
	runner := &testRunner{started: make(chan RunRequest, 1)}
	supervisor, store, root := newSupervisorTestWithRunning(t, nil, 1, 4, 3, 4)
	child, err := store.CreateChild(ctx, childSpec(root, "before-cancel", "must stay stopped"))
	if err != nil {
		t.Fatal(err)
	}
	team, err := store.GetTeam(ctx, root.TeamID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionTeam(ctx, orchestration.TeamTransition{TeamID: root.TeamID, Expected: team.Version, Target: orchestration.TeamCanceling}); err != nil {
		t.Fatal(err)
	}
	supervisor.runner = runner
	if err := supervisor.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-runner.started:
		t.Fatalf("canceled Team admitted %s", request.Agent.ID)
	case <-time.After(100 * time.Millisecond):
	}
	waitCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if team, err := supervisor.CancelTeam(waitCtx, root.TeamID, "finish interrupted cancellation"); err != nil || team.State != orchestration.TeamCanceled {
		t.Fatalf("finish cancellation = %#v, err=%v", team, err)
	}
	if child, err := store.GetAgent(ctx, child.ID); err != nil || child.State != orchestration.AgentCanceled {
		t.Fatalf("child = %#v, err=%v", child, err)
	}
}

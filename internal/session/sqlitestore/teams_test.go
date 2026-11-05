package sqlitestore

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

func TestTeamTransitionsGateSpawnAndReopenWithTheSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	seed := orchestration.RootThreadSeed{SessionID: session.ID("team-session")}
	root, _, err := store.EnsureRootThread(ctx, seed)
	if err != nil {
		t.Fatal(err)
	}
	team, err := store.GetTeam(ctx, root.TeamID)
	if err != nil || team.State != orchestration.TeamActive || team.RootAgentID != root.AgentID {
		t.Fatalf("team = %#v, err=%v", team, err)
	}
	// A stale version and an invalid target are rejected.
	if _, err := store.TransitionTeam(ctx, orchestration.TeamTransition{TeamID: root.TeamID, Expected: team.Version + 1, Target: orchestration.TeamCanceling}); !errors.Is(err, orchestration.ErrTeamStateConflict) {
		t.Fatalf("stale transition error = %v", err)
	}
	if _, err := store.TransitionTeam(ctx, orchestration.TeamTransition{TeamID: root.TeamID, Expected: team.Version, Target: orchestration.TeamCanceled}); err == nil {
		t.Fatal("active -> canceled accepted")
	}
	for _, target := range []orchestration.TeamState{orchestration.TeamCanceling, orchestration.TeamCanceled} {
		if team, err = store.TransitionTeam(ctx, orchestration.TeamTransition{TeamID: root.TeamID, Expected: team.Version, Target: target, Reason: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	spec := orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker-v1", InitialPrompt: "work"}
	if _, err := store.CreateChild(ctx, spec); !errors.Is(err, orchestration.ErrTeamNotActive) {
		t.Fatalf("spawn in canceled Team error = %v", err)
	}
	// Resuming the session reopens the Team.
	resumed, _, err := store.EnsureRootThread(ctx, seed)
	if err != nil || resumed.TeamState != orchestration.TeamActive {
		t.Fatalf("resumed = %#v, err=%v", resumed, err)
	}
	if _, err := store.CreateChild(ctx, spec); err != nil {
		t.Fatalf("spawn in reopened Team: %v", err)
	}
	events, err := store.LoadTeam(ctx, root.TeamID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var teamEvents []orchestration.EventType
	for _, event := range events {
		if event.StreamType == orchestration.StreamTeam {
			teamEvents = append(teamEvents, event.Type)
		}
	}
	want := []orchestration.EventType{orchestration.EventTeamCreated, orchestration.EventTeamCancelRequested, orchestration.EventTeamCanceled, orchestration.EventTeamReopened}
	if !slices.Equal(teamEvents, want) {
		t.Fatalf("team events = %q, want %q", teamEvents, want)
	}
}

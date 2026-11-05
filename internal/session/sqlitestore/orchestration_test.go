package sqlitestore

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

func TestOrchestrationStoreAppendLoadAndCursor(t *testing.T) {
	t.Parallel()
	database, err := Open(context.Background(), filepath.Join(t.TempDir(), "state", "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	teamID, err := orchestration.NewTeamID()
	if err != nil {
		t.Fatal(err)
	}
	stream := orchestration.Stream{Type: orchestration.StreamTeam, ID: string(teamID), TeamID: teamID}
	store := database.Orchestration()
	appended, err := store.Append(context.Background(), stream, 0,
		orchestration.PendingEvent{Type: orchestration.EventTeamCreated, Payload: orchestration.MustJSONPayload(map[string]string{"state": "active"})},
		orchestration.PendingEvent{Type: orchestration.EventTeamDraining, Payload: orchestration.MustJSONPayload(map[string]string{"state": "draining"})},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(appended) != 2 || appended[0].StreamSequence != 1 || appended[1].StreamSequence != 2 {
		t.Fatalf("unexpected stream sequence: %#v", appended)
	}
	if appended[0].GlobalSequence == 0 || appended[1].GlobalSequence != appended[0].GlobalSequence+1 {
		t.Fatalf("unexpected global sequence: %#v", appended)
	}
	if appended[0].Checksum == "" {
		t.Fatal("checksum must be populated")
	}
	if _, err := store.Append(context.Background(), stream, 0, orchestration.PendingEvent{
		Type: orchestration.EventTeamCompleted, Payload: orchestration.MustJSONPayload(map[string]string{}),
	}); !errors.Is(err, orchestration.ErrSequenceConflict) {
		t.Fatalf("got %v, want sequence conflict", err)
	}
	loaded, err := store.LoadStream(context.Background(), orchestration.StreamTeam, string(teamID), 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].Type != orchestration.EventTeamDraining {
		t.Fatalf("unexpected stream load: %#v", loaded)
	}
	global, err := store.LoadGlobal(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(global) != 2 || global[0].EventID != appended[0].EventID {
		t.Fatalf("unexpected global load: %#v", global)
	}
}

func TestEnsureRootThreadIsIdempotent(t *testing.T) {
	t.Parallel()
	database, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	seed := orchestration.RootThreadSeed{SessionID: session.ID("legacy-session"), ProfileRevision: "default-v1"}
	first, created, err := store.EnsureRootThread(context.Background(), seed)
	if err != nil {
		t.Fatal(err)
	}
	if !created || first.TeamID == "" || first.AgentID == "" || first.ThreadID == "" {
		t.Fatalf("unexpected root: %#v, created=%v", first, created)
	}
	second, created, err := store.EnsureRootThread(context.Background(), seed)
	if err != nil {
		t.Fatal(err)
	}
	if created || second.TeamID != first.TeamID || second.AgentID != first.AgentID || second.ThreadID != first.ThreadID {
		t.Fatalf("root was not reused: first=%#v second=%#v created=%v", first, second, created)
	}
	events, err := store.LoadGlobal(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 {
		t.Fatalf("got %d root events, want 4", len(events))
	}
}

func TestOrchestrationStoreCreatesAndTransitionsChild(t *testing.T) {
	t.Parallel()
	database, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: session.ID("root-session")})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.CreateChild(context.Background(), orchestration.SpawnSpec{
		TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "explorer-v1", InitialPrompt: "child task", ContextPolicy: "summary", IdempotencyKey: "spawn-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if child.ParentAgentID != root.AgentID || child.Depth != 1 || child.State != orchestration.AgentQueued || child.SessionID == "" || child.ContextPolicy != "summary" || child.InitialPrompt != "child task" {
		t.Fatalf("unexpected child: %#v", child)
	}
	repeated, err := store.CreateChild(context.Background(), orchestration.SpawnSpec{
		TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "explorer-v1", InitialPrompt: "child task", ContextPolicy: "summary", IdempotencyKey: "spawn-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if repeated.ID != child.ID {
		t.Fatalf("idempotency returned %s, want %s", repeated.ID, child.ID)
	}
	if _, err := store.CreateChild(context.Background(), orchestration.SpawnSpec{
		TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "explorer-v1", InitialPrompt: "different task", ContextPolicy: "summary", IdempotencyKey: "spawn-1",
	}); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("got %v, want idempotency conflict", err)
	}
	agents, err := store.ListAgents(context.Background(), orchestration.AgentQuery{TeamID: root.TeamID})
	if err != nil || len(agents) != 2 {
		t.Fatalf("agents=%#v err=%v", agents, err)
	}
	runID, err := orchestration.NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	updated, err := store.TransitionAgent(context.Background(), orchestration.AgentTransition{
		AgentID: child.ID, Expected: child.Version, Target: orchestration.AgentRunning, RunID: runID,
		Activity: "model",
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.State != orchestration.AgentRunning || updated.Version != child.Version+1 {
		t.Fatalf("unexpected transition: %#v", updated)
	}
	if _, err := store.TransitionAgent(context.Background(), orchestration.AgentTransition{AgentID: child.ID, Expected: child.Version, Target: orchestration.AgentCompleted}); !errors.Is(err, orchestration.ErrAgentStateConflict) {
		t.Fatalf("got %v, want Agent state conflict", err)
	}
}

func TestDatabaseMigrationIsIdempotent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state", "agent.db")
	first, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSpawnHashKeepsAutoWorkspaceCompatible(t *testing.T) {
	spec := orchestration.SpawnSpec{TeamID: "team_x", ParentAgentID: "agent_y", ProfileRevision: "worker-v1", InitialPrompt: "task"}
	recorded := hashSpawnRequest(spec)
	spec.Workspace = orchestration.WorkspaceAuto
	if hashSpawnRequest(spec) != recorded {
		t.Fatal("auto workspace changed the identity of spawns recorded before workspace access existed")
	}
	spec.Workspace = orchestration.WorkspaceReadonly
	if hashSpawnRequest(spec) == recorded {
		t.Fatal("read-only workspace access is not part of the spawn identity")
	}
}

func TestChildWorkspaceIdentityRoundTrips(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "workspace-identity"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.CreateChild(ctx, orchestration.SpawnSpec{
		TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker-v1",
		Workspace: orchestration.WorkspaceReadonly, WorkspaceMode: orchestration.WorkspaceModeSharedReadonly,
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.GetAgent(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Workspace != orchestration.WorkspaceReadonly || loaded.WorkspaceMode != orchestration.WorkspaceModeSharedReadonly {
		t.Fatalf("workspace identity = %q %q", loaded.Workspace, loaded.WorkspaceMode)
	}
	implicit, err := store.CreateChild(ctx, orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker-v1"})
	if err != nil || implicit.Workspace != orchestration.WorkspaceAuto {
		t.Fatalf("omitted workspace access = %q, %v", implicit.Workspace, err)
	}
	if _, err := store.CreateChild(ctx, orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker-v1", WorkspaceMode: "everything"}); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("unknown workspace mode was stored: %v", err)
	}
}

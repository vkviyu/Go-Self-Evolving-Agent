package agentmanager

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentprofile"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/sqlitestore"
)

func managerProfile(name string) agentprofile.Profile {
	return agentprofile.Profile{Name: name, Revision: name + "-v1", ModelProfile: "test", Instruction: "original instruction", ContextInheritance: agentprofile.InheritSummary, WorkspaceMode: agentprofile.WorkspaceSharedLease, CanSpawn: true, Limits: agentprofile.Limits{MaxIterations: 5, MaxToolCalls: 10, MaxChildren: 4, MaxInheritedTokens: 20, MaxWallTime: time.Minute}}
}

func profileSupervisor(t *testing.T, path string, parent agentprofile.Profile, runner Runner) (*Supervisor, *sqlitestore.Database, orchestration.RootThread) {
	t.Helper()
	ctx := context.Background()
	database, err := sqlitestore.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	snapshot, err := agentprofile.Encode(parent)
	if err != nil {
		t.Fatal(err)
	}
	root, _, err := database.Orchestration().EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "profile-root", ProfileRevision: parent.Revision, ProfileSnapshot: snapshot})
	if err != nil {
		t.Fatal(err)
	}
	profiles := agentprofile.NewRegistry()
	if _, err := profiles.Activate(parent, managerProfile("worker")); err != nil {
		t.Fatal(err)
	}
	supervisor, err := NewSupervisor(Config{Agents: database.Orchestration(), Profiles: profiles, Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Close() })
	return supervisor, database, root
}

func TestUnknownSpawnProfileNamesTheAvailableProfiles(t *testing.T) {
	supervisor, _, root := profileSupervisor(t, filepath.Join(t.TempDir(), "profiles.db"), managerProfile("root"), nil)
	_, err := supervisor.Spawn(context.Background(), orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileName: "general", InitialPrompt: "task"})
	if !errors.Is(err, orchestration.ErrNotFound) || !strings.Contains(err.Error(), "available: root, worker") {
		t.Fatalf("unknown profile error = %v", err)
	}
}

func TestDurableAgentProfileSurvivesDatabaseAndRegistryRecreation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "profiles.db")
	parent := managerProfile("root")
	parent.Tools, parent.DelegableTools = []string{"read", "agent_spawn"}, []string{"read"}
	supervisor, database, root := profileSupervisor(t, path, parent, nil)
	child, err := supervisor.Spawn(ctx, orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileName: "worker", InitialPrompt: "task", ProfileSnapshot: json.RawMessage(`{"forged":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Close(); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, reopened, _ := profileSupervisor(t, path, parent, nil)
	// A different registry after restart must not widen an existing Agent.
	changed := managerProfile("worker")
	changed.Instruction, changed.CanSendMessages = "replacement instruction", true
	restarted.profiles = agentprofile.NewRegistry()
	if _, err := restarted.profiles.Activate(changed); err != nil {
		t.Fatal(err)
	}
	loaded, err := reopened.Orchestration().GetAgent(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	profile, ok, err := restarted.actorProfile(ctx, loaded.ActorScope())
	if err != nil || !ok || profile.Instruction != "original instruction" || profile.CanSendMessages || !slices.Equal(profile.Tools, []string{"read"}) {
		t.Fatalf("durable profile drifted: %#v %v", profile, err)
	}
	agents, err := reopened.Orchestration().ListAgents(ctx, orchestration.AgentQuery{TeamID: root.TeamID})
	if err != nil {
		t.Fatal(err)
	}
	for _, agent := range agents {
		if _, err := agentprofile.Decode(agent.ProfileSnapshot, agent.ProfileRevision); err != nil {
			t.Fatalf("projection omitted snapshot: %s %v", agent.ID, err)
		}
	}
	events, err := reopened.Orchestration().LoadGlobal(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	created := 0
	for _, event := range events {
		if event.Type != orchestration.EventAgentCreated {
			continue
		}
		var payload struct {
			Revision string          `json:"profile_revision"`
			Profile  json.RawMessage `json:"profile_snapshot"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if _, err := agentprofile.Decode(payload.Profile, payload.Revision); err != nil {
			t.Fatalf("creation event omitted profile: %v", err)
		}
		created++
	}
	if created != 2 {
		t.Fatalf("created events=%d", created)
	}
}

func TestSupervisorHonorsZeroChildrenAndContextCeiling(t *testing.T) {
	ctx := context.Background()
	parent := managerProfile("root")
	parent.Limits.MaxChildren = 0
	supervisor, _, root := profileSupervisor(t, ":memory:", parent, nil)
	if _, err := supervisor.Spawn(ctx, childSpec(root, "zero", "task")); !errors.Is(err, orchestration.ErrAgentLimit) {
		t.Fatalf("zero means unlimited: %v", err)
	}
	parent.Limits.MaxChildren = 4
	supervisor, _, root = profileSupervisor(t, ":memory:", parent, nil)
	spec := childSpec(root, "context", "task")
	spec.ContextPolicy = "full"
	if _, err := supervisor.Spawn(ctx, spec); err == nil || !strings.Contains(err.Error(), "inheritance") {
		t.Fatalf("context widening allowed: %v", err)
	}
	spec.ContextPolicy = "summary"
	supervisor.seeder = ContextSeederFunc(func(context.Context, orchestration.AgentSnapshot, string) (string, error) { return "unbounded", nil })
	if _, err := supervisor.Spawn(ctx, spec); err == nil || !strings.Contains(err.Error(), "token bound") {
		t.Fatalf("unbounded seeder accepted: %v", err)
	}
	seed := &boundedProfileSeeder{}
	supervisor.seeder = seed
	child, err := supervisor.Spawn(ctx, spec)
	if err != nil || seed.limit != 20 || !strings.Contains(child.InitialPrompt, "bounded") {
		t.Fatalf("bounded seed limit=%d child=%#v err=%v", seed.limit, child, err)
	}
}

type boundedProfileSeeder struct{ limit int }

func (*boundedProfileSeeder) Seed(context.Context, orchestration.AgentSnapshot, string) (string, error) {
	return "", errors.New("unbounded method must not be called")
}
func (seeder *boundedProfileSeeder) SeedWithProfile(_ context.Context, _ orchestration.AgentSnapshot, _ string, profile agentprofile.Profile) (string, error) {
	seeder.limit = profile.Limits.MaxInheritedTokens
	return "bounded", nil
}

func TestSupervisorRunDeadlineUsesDurableCreationTime(t *testing.T) {
	type observed struct {
		deadline time.Time
		created  time.Time
	}
	started := make(chan observed, 1)
	runner := RunnerFunc(func(ctx context.Context, request RunRequest) (orchestration.AgentResult, error) {
		deadline, _ := ctx.Deadline()
		started <- observed{deadline, request.Agent.CreatedAt}
		<-ctx.Done()
		return orchestration.AgentResult{}, ctx.Err()
	})
	supervisor, _, root := profileSupervisor(t, ":memory:", managerProfile("root"), runner)
	child, err := supervisor.Spawn(context.Background(), childSpec(root, "deadline", "task"))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-started:
		if !result.deadline.Equal(result.created.Add(time.Minute)) {
			t.Fatalf("deadline reset: %#v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("child did not start")
	}
	if err := supervisor.Interrupt(context.Background(), child.ID); err != nil {
		t.Fatal(err)
	}
}

func TestSpawnWorkspaceRequestIsDurableIdentity(t *testing.T) {
	ctx := context.Background()
	supervisor, database, root := profileSupervisor(t, ":memory:", managerProfile("root"), nil)
	spec := orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileName: "worker", InitialPrompt: "inspect", Workspace: orchestration.WorkspaceReadonly, IdempotencyKey: "readonly-child"}
	child, err := supervisor.Spawn(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := database.Orchestration().GetAgent(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Workspace != orchestration.WorkspaceReadonly || loaded.WorkspaceMode != orchestration.WorkspaceModeSharedReadonly {
		t.Fatalf("workspace identity was not persisted: %q %q", loaded.Workspace, loaded.WorkspaceMode)
	}
	profile, err := agentprofile.Decode(loaded.ProfileSnapshot, loaded.ProfileRevision)
	if err != nil || profile.WorkspaceMode != agentprofile.WorkspaceSharedReadonly {
		t.Fatalf("durable profile kept write access: %#v %v", profile, err)
	}
	retry := spec
	retry.Workspace = orchestration.WorkspaceAuto
	if _, err := supervisor.Spawn(ctx, retry); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("retry with different workspace access reused the child: %v", err)
	}
	if replay, err := supervisor.Spawn(ctx, spec); err != nil || replay.ID != child.ID {
		t.Fatalf("identical retry = %#v, %v", replay, err)
	}
	unsupported := spec
	unsupported.Workspace, unsupported.IdempotencyKey = "snapshot", "snapshot-child"
	if _, err := supervisor.Spawn(ctx, unsupported); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("unimplemented workspace access was accepted: %v", err)
	}
}

func TestReadonlyParentSpawnsOnlyReadonlyChildren(t *testing.T) {
	ctx := context.Background()
	parent := managerProfile("root")
	parent.WorkspaceMode = agentprofile.WorkspaceSharedReadonly
	supervisor, _, root := profileSupervisor(t, ":memory:", parent, nil)
	spec := orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileName: "worker", InitialPrompt: "inspect"}
	if _, err := supervisor.Spawn(ctx, spec); err == nil {
		t.Fatal("read-only parent delegated a writable workspace")
	}
	spec.Workspace = orchestration.WorkspaceReadonly
	child, err := supervisor.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("read-only request was not narrowed before delegation: %v", err)
	}
	if child.WorkspaceMode != orchestration.WorkspaceModeSharedReadonly {
		t.Fatalf("granted mode = %q", child.WorkspaceMode)
	}
}

func TestExplicitSnapshotProfileDoesNotFallBackToSharedDefault(t *testing.T) {
	profile := managerProfile("isolated")
	profile.WorkspaceMode = agentprofile.WorkspaceSnapshot
	if mode := grantWorkspaceMode(orchestration.WorkspaceAuto, profile, true, orchestration.WorkspaceModeSharedLease); mode != orchestration.WorkspaceModeSnapshot {
		t.Fatalf("snapshot profile downgraded to %q", mode)
	}
	if mode := grantWorkspaceMode(orchestration.WorkspaceReadonly, profile, true, orchestration.WorkspaceModeSharedLease); mode != orchestration.WorkspaceModeSharedReadonly {
		t.Fatalf("explicit readonly request granted %q", mode)
	}
}

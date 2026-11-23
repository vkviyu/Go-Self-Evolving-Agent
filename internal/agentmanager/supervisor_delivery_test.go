package agentmanager

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentprofile"
	"github.com/vkviyu/go-self-evolving-agent/internal/artifact"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/testfixture"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

type (
	// snapshotTeam runs a Supervisor whose writers work in snapshots of a
	// temporary main workspace and merge without approval.
	snapshotTeam struct {
		t          *testing.T
		ctx        context.Context
		store      *testfixture.OrchestrationStore
		root       orchestration.RootThread
		main       string
		supervisor *Supervisor
	}

	// snapshotRunner runs each Agent with work inside its active view.
	snapshotRunner struct {
		store *testfixture.OrchestrationStore
		work  func(ctx context.Context, request RunRequest, view workspacecoord.View) error
	}

	// gate releases its callers once all expected callers arrived, so
	// parallel workers provably overlap.
	gate struct {
		arrived sync.WaitGroup
	}

	// observations records what Agents saw while they ran.
	observations struct {
		mu     sync.Mutex
		values map[string]string
	}
)

func (runner *snapshotRunner) Run(ctx context.Context, request RunRequest) (orchestration.AgentResult, error) {
	views, err := runner.store.ListViews(ctx, workspacecoord.ViewQuery{
		TeamID: request.Agent.TeamID, AgentID: request.Agent.ID, States: []workspacecoord.ViewState{workspacecoord.ViewActive},
	})
	if err != nil {
		return orchestration.AgentResult{}, err
	}
	if len(views) == 0 {
		return orchestration.AgentResult{}, errors.New("Agent has no active workspace view")
	}
	if err := runner.work(ctx, request, views[len(views)-1]); err != nil {
		return orchestration.AgentResult{}, err
	}
	return orchestration.AgentResult{Status: orchestration.ResultSucceeded, Summary: "done"}, nil
}

// newGate expects count callers.
func newGate(count int) *gate {
	waiters := &gate{}
	waiters.arrived.Add(count)
	return waiters
}

// pass waits until every expected caller arrived.
func (waiters *gate) pass(ctx context.Context) error {
	waiters.arrived.Done()
	done := make(chan struct{})
	go func() {
		waiters.arrived.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Second):
		return errors.New("gate timed out")
	}
}

// record stores what name saw.
func (seen *observations) record(name, value string) {
	seen.mu.Lock()
	defer seen.mu.Unlock()
	if seen.values == nil {
		seen.values = make(map[string]string)
	}
	seen.values[name] = value
}

// get returns what name saw.
func (seen *observations) get(name string) string {
	seen.mu.Lock()
	defer seen.mu.Unlock()
	return seen.values[name]
}

// newSnapshotTeam creates a main workspace holding files and a Supervisor
// that isolates writers in snapshots. A root profile requires profiles.
func newSnapshotTeam(t *testing.T, files map[string]string, profiles *agentprofile.Registry, rootProfile *agentprofile.Profile, runner *snapshotRunner) *snapshotTeam {
	t.Helper()
	ctx := context.Background()
	database, err := testfixture.Open(t, ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	seed := orchestration.RootThreadSeed{SessionID: "snapshot-delivery-root"}
	if rootProfile != nil {
		snapshot, err := agentprofile.Encode(*rootProfile)
		if err != nil {
			t.Fatal(err)
		}
		seed.ProfileRevision, seed.ProfileSnapshot = rootProfile.Revision, snapshot
	}
	root, _, err := store.EnsureRootThread(ctx, seed)
	if err != nil {
		t.Fatal(err)
	}
	main, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		writeTeamFile(t, filepath.Join(main, name), content)
	}
	data := t.TempDir()
	artifacts, err := artifact.NewStore(filepath.Join(data, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := workspacecoord.NewCoordinator(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	coordinator.Artifacts, coordinator.Events = artifacts, store
	coordinator.Snapshots = &workspacecoord.SnapshotStore{ViewsDirectory: filepath.Join(data, "views"), WorkspaceRoot: main}
	executor, err := workspacecoord.NewMergeExecutor(store, coordinator, nil, artifacts, store)
	if err != nil {
		t.Fatal(err)
	}
	executor.AutoMerge = true
	runner.store = store
	supervisor, err := NewSupervisor(Config{
		Agents: store, Tasks: store, Mailbox: store, Leases: newLeaseStore(t), Profiles: profiles, Runner: runner,
		Workspace: coordinator, MergeExecutor: executor, WorkspaceRoot: main,
		MaxRunningAgents: 3, MaxLiveAgents: 8, MaxDepth: 3, MaxChildren: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Close() })
	return &snapshotTeam{t: t, ctx: ctx, store: store, root: root, main: main, supervisor: supervisor}
}

// create stores a draft task of the team.
func (team *snapshotTeam) create(spec orchestration.TaskSpec) orchestration.TaskSnapshot {
	team.t.Helper()
	spec.TeamID = team.root.TeamID
	task, err := team.store.CreateTask(team.ctx, spec)
	if err != nil {
		team.t.Fatal(err)
	}
	return task
}

// ready makes task ready, which dispatches it.
func (team *snapshotTeam) ready(task orchestration.TaskSnapshot) {
	team.t.Helper()
	if _, err := team.supervisor.TransitionTask(team.ctx, orchestration.TaskTransition{
		TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskReady, ActorAgentID: team.root.AgentID,
	}); err != nil {
		team.t.Fatal(err)
	}
}

// task loads a task by ID.
func (team *snapshotTeam) task(id orchestration.TaskID) orchestration.TaskSnapshot {
	team.t.Helper()
	task, err := team.store.GetTask(team.ctx, id)
	if err != nil {
		team.t.Fatal(err)
	}
	return task
}

// waitTask polls until the task reaches state.
func (team *snapshotTeam) waitTask(id orchestration.TaskID, state orchestration.TaskState) orchestration.TaskSnapshot {
	team.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		task := team.task(id)
		if task.State == state {
			return task
		}
		if time.Now().After(deadline) {
			team.t.Fatalf("task %s (%s) state = %s, want %s", task.ID, task.Title, task.State, state)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// worker returns the Agent dispatched for task.
func (team *snapshotTeam) worker(id orchestration.TaskID) orchestration.AgentSnapshot {
	team.t.Helper()
	agents, err := team.store.ListAgents(team.ctx, orchestration.AgentQuery{TeamID: team.root.TeamID, ParentAgentID: team.root.AgentID})
	if err != nil {
		team.t.Fatal(err)
	}
	for _, agent := range agents {
		if agent.AssignedTaskID == id {
			return agent
		}
	}
	team.t.Fatalf("no Agent was dispatched for task %s", id)
	return orchestration.AgentSnapshot{}
}

// assignedTitle returns the title of the task the request's Agent works on.
func (runner *snapshotRunner) assignedTitle(ctx context.Context, request RunRequest) (string, error) {
	if request.Agent.AssignedTaskID == "" {
		return "", nil
	}
	task, err := runner.store.GetTask(ctx, request.Agent.AssignedTaskID)
	return task.Title, err
}

func writeTeamFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readTeamFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func TestSnapshotWorkersMergeBeforeDependentTaskStarts(t *testing.T) {
	const base = "one\ntwo\nthree\nfour\nfive\n"
	both := newGate(2)
	var seen observations
	runner := &snapshotRunner{}
	runner.work = func(ctx context.Context, request RunRequest, view workspacecoord.View) error {
		title, err := runner.assignedTitle(ctx, request)
		if err != nil {
			return err
		}
		path := filepath.Join(view.RootPath, "a.txt")
		switch title {
		case "first", "second":
			if view.Mode != workspacecoord.ViewSnapshot {
				return errors.New("writer did not get a snapshot view")
			}
			// Both writers hold their snapshots at the same time.
			if err := both.pass(ctx); err != nil {
				return err
			}
			content := strings.Replace(base, "one", "ONE", 1)
			if title == "second" {
				content = strings.Replace(base, "five", "FIVE", 1)
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				return err
			}
		case "consume":
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			seen.record(title, string(content))
		}
		return nil
	}
	team := newSnapshotTeam(t, map[string]string{"a.txt": base}, nil, nil, runner)
	first := team.create(orchestration.TaskSpec{Title: "first"})
	second := team.create(orchestration.TaskSpec{Title: "second"})
	consume := team.create(orchestration.TaskSpec{Title: "consume"})
	for _, dependency := range []orchestration.TaskID{first.ID, second.ID} {
		if err := team.store.AddDependency(team.ctx, consume.ID, dependency, team.root.AgentID, "consume-after-"+string(dependency)); err != nil {
			t.Fatal(err)
		}
	}
	team.ready(first)
	team.ready(team.task(second.ID))
	team.waitTask(consume.ID, orchestration.TaskCompleted)
	const merged = "ONE\ntwo\nthree\nfour\nFIVE\n"
	if content := seen.get("consume"); content != merged {
		t.Fatalf("dependent task saw %q, want both merged edits", content)
	}
	if content := readTeamFile(t, filepath.Join(team.main, "a.txt")); content != merged {
		t.Fatalf("main = %q, want both edits", content)
	}
	for _, task := range []orchestration.TaskSnapshot{first, second} {
		team.waitTask(task.ID, orchestration.TaskCompleted)
		worker := team.worker(task.ID)
		if worker.Result == nil || worker.Result.Delivery == nil || worker.Result.Delivery.MergeID == "" || worker.Result.Delivery.CandidateDigest == "" {
			t.Fatalf("worker result of %s has no delivery: %#v", task.Title, worker.Result)
		}
		merge, err := team.store.GetMerge(team.ctx, worker.Result.Delivery.MergeID)
		if err != nil {
			t.Fatal(err)
		}
		if merge.State != workspacecoord.MergeApplied || merge.TaskID != task.ID {
			t.Fatalf("merge of %s = %#v", task.Title, merge)
		}
	}
}

func TestSnapshotConflictIsResolvedByDispatchedResolutionTask(t *testing.T) {
	const base = "one\ntwo\nthree\n"
	both := newGate(2)
	var seen observations
	runner := &snapshotRunner{}
	runner.work = func(ctx context.Context, request RunRequest, view workspacecoord.View) error {
		title, err := runner.assignedTitle(ctx, request)
		if err != nil {
			return err
		}
		path := filepath.Join(view.RootPath, "a.txt")
		switch {
		case title == "first" || title == "second":
			if err := both.pass(ctx); err != nil {
				return err
			}
			return os.WriteFile(path, []byte("one\nTWO-"+title+"\nthree\n"), 0o644)
		case strings.HasPrefix(title, "Resolve merge conflict "):
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			seen.record("resolver", string(content))
			return os.WriteFile(path, []byte("one\nTWO-both\nthree\n"), 0o644)
		}
		return nil
	}
	team := newSnapshotTeam(t, map[string]string{"a.txt": base}, nil, nil, runner)
	first := team.create(orchestration.TaskSpec{Title: "first"})
	second := team.create(orchestration.TaskSpec{Title: "second"})
	team.ready(first)
	team.ready(second)
	team.waitTask(first.ID, orchestration.TaskCompleted)
	team.waitTask(second.ID, orchestration.TaskCompleted)
	seeded := seen.get("resolver")
	if !workspacecoord.HasConflictMarkers(seeded) || !strings.Contains(seeded, "TWO-first") || !strings.Contains(seeded, "TWO-second") {
		t.Fatalf("resolver work tree = %q, want both sides with conflict markers", seeded)
	}
	if content := readTeamFile(t, filepath.Join(team.main, "a.txt")); content != "one\nTWO-both\nthree\n" {
		t.Fatalf("main = %q, want the resolution", content)
	}
	merges, err := team.store.ListMerges(team.ctx, workspacecoord.MergeQuery{TeamID: team.root.TeamID})
	if err != nil {
		t.Fatal(err)
	}
	states := make(map[workspacecoord.MergeState]int)
	var resolution orchestration.TaskID
	for _, merge := range merges {
		states[merge.State]++
		if merge.ResolutionTaskID != "" {
			resolution = merge.ResolutionTaskID
		}
	}
	if states[workspacecoord.MergeApplied] != 2 || states[workspacecoord.MergeSuperseded] != 1 || len(merges) != 3 {
		t.Fatalf("merge states = %v, want two applied and one superseded", states)
	}
	if resolution == "" {
		t.Fatal("conflicted merge has no resolution task")
	}
	team.waitTask(resolution, orchestration.TaskCompleted)
}

func TestReviewerSeesSnapshotCandidateBeforeItMerges(t *testing.T) {
	limits := agentprofile.Limits{MaxIterations: 1, MaxToolCalls: 1, MaxChildren: 4, MaxWallTime: time.Minute}
	root := agentprofile.Profile{Name: "lead", Revision: "lead-v1", ModelProfile: "test", ContextInheritance: agentprofile.InheritSummary, WorkspaceMode: agentprofile.WorkspaceSharedLease, Limits: limits, CanSpawn: true, CanManageTasks: true}
	profiles := agentprofile.NewRegistry()
	if _, err := profiles.Activate(
		root,
		agentprofile.Profile{Name: "worker", Revision: "worker-v1", ModelProfile: "test", ContextInheritance: agentprofile.InheritSummary, WorkspaceMode: agentprofile.WorkspaceSharedLease, Limits: limits, CanManageTasks: true},
		agentprofile.Profile{Name: "reviewer", Revision: "reviewer-v1", ModelProfile: "test", ContextInheritance: agentprofile.InheritSummary, WorkspaceMode: agentprofile.WorkspaceSharedReadonly, Limits: limits, CanManageTasks: true},
	); err != nil {
		t.Fatal(err)
	}
	var (
		seen observations
		// verify and main are set before any Agent runs.
		verify func(context.Context, json.RawMessage) (json.RawMessage, error)
		main   string
	)
	runner := &snapshotRunner{}
	runner.work = func(ctx context.Context, request RunRequest, view workspacecoord.View) error {
		path := filepath.Join(view.RootPath, "a.txt")
		if request.Agent.AssignedTaskID != "" {
			seen.record("worker root", view.RootPath)
			return os.WriteFile(path, []byte("reviewed\n"), 0o644)
		}
		candidate, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		current, err := os.ReadFile(filepath.Join(main, "a.txt"))
		if err != nil {
			return err
		}
		tasks, err := runner.store.ListTasks(ctx, orchestration.TaskQuery{TeamID: request.Agent.TeamID, States: []orchestration.TaskState{orchestration.TaskVerification}})
		if err != nil {
			return err
		}
		if len(tasks) != 1 {
			return errors.New("reviewer found no task in verification")
		}
		seen.record("mode", string(view.Mode))
		seen.record("reviewer root", view.RootPath)
		seen.record("candidate", string(candidate))
		seen.record("main", string(current))
		seen.record("prompt", request.Agent.InitialPrompt)
		actor := orchestration.WithActorScope(ctx, request.Agent.ActorScope())
		_, err = verify(actor, json.RawMessage(`{"task_id":"`+string(tasks[0].ID)+`","passed":true}`))
		return err
	}
	team := newSnapshotTeam(t, map[string]string{"a.txt": "base\n"}, profiles, &root, runner)
	main = team.main
	for _, binding := range Bindings(team.supervisor) {
		if binding.Descriptor.Name == "task_verify" {
			verify = binding.Executor.Invoke
		}
	}
	task := team.create(orchestration.TaskSpec{Title: "change a", RequiredProfile: "worker", VerificationProfile: "reviewer"})
	team.ready(task)
	team.waitTask(task.ID, orchestration.TaskCompleted)
	if mode := seen.get("mode"); mode != string(workspacecoord.ViewSharedReadonly) {
		t.Fatalf("reviewer view mode = %q, want shared read-only", mode)
	}
	if reviewer, worker := seen.get("reviewer root"), seen.get("worker root"); reviewer == "" || reviewer != worker {
		t.Fatalf("reviewer root = %q, want the worker work tree %q", reviewer, worker)
	}
	if candidate, current := seen.get("candidate"), seen.get("main"); candidate != "reviewed\n" || current != "base\n" {
		t.Fatalf("reviewer saw candidate %q and main %q", candidate, current)
	}
	if prompt := seen.get("prompt"); !strings.Contains(prompt, "Candidate diff") || !strings.Contains(prompt, "+reviewed") {
		t.Fatalf("reviewer prompt has no candidate diff: %q", prompt)
	}
	if content := readTeamFile(t, filepath.Join(team.main, "a.txt")); content != "reviewed\n" {
		t.Fatalf("main = %q, want the reviewed candidate", content)
	}
}

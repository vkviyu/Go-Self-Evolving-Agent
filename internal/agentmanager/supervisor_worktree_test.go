package agentmanager

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentprofile"
	"github.com/vkviyu/go-self-evolving-agent/internal/artifact"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/testfixture"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

// runTeamGit runs git in directory without the user's configuration.
func runTeamGit(t *testing.T, directory string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
}

func TestWorktreeWorkerPatchMergesAfterItsTaskAwaitsMerge(t *testing.T) {
	ctx := context.Background()
	main, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runTeamGit(t, main, "init", "-q")
	runTeamGit(t, main, "config", "user.name", "Test")
	runTeamGit(t, main, "config", "user.email", "test@example.invalid")
	writeTeamFile(t, filepath.Join(main, "a.txt"), "base\n")
	writeTeamFile(t, filepath.Join(main, ".gitignore"), "/.gsea/\n")
	runTeamGit(t, main, "add", ".")
	runTeamGit(t, main, "commit", "-qm", "base")
	database, err := testfixture.Open(t, ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	limits := agentprofile.Limits{MaxIterations: 1, MaxToolCalls: 1, MaxChildren: 4, MaxWallTime: time.Minute}
	lead := agentprofile.Profile{Name: "lead", Revision: "lead-v1", ModelProfile: "test", ContextInheritance: agentprofile.InheritSummary, WorkspaceMode: agentprofile.WorkspaceSharedLease, Limits: limits, CanSpawn: true, CanManageTasks: true}
	profiles := agentprofile.NewRegistry()
	if _, err := profiles.Activate(lead, agentprofile.Profile{
		Name: "worker", Revision: "worker-v1", ModelProfile: "test", ContextInheritance: agentprofile.InheritSummary,
		WorkspaceMode: agentprofile.WorkspaceGitWorktree, Limits: limits, CanManageTasks: true,
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := agentprofile.Encode(lead)
	if err != nil {
		t.Fatal(err)
	}
	root, _, err := store.EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "worktree-delivery-root", ProfileRevision: lead.Revision, ProfileSnapshot: snapshot})
	if err != nil {
		t.Fatal(err)
	}
	worktrees, err := workspacecoord.NewWorktreeManager(main)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := artifact.NewStore(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := workspacecoord.NewCoordinator(store, worktrees)
	if err != nil {
		t.Fatal(err)
	}
	coordinator.Artifacts, coordinator.Events = artifacts, store
	executor, err := workspacecoord.NewMergeExecutor(store, coordinator, worktrees, artifacts, store)
	if err != nil {
		t.Fatal(err)
	}
	executor.AutoMerge = true
	var seen observations
	runner := &snapshotRunner{store: store, work: func(_ context.Context, _ RunRequest, view workspacecoord.View) error {
		seen.record("mode", string(view.Mode))
		return os.WriteFile(filepath.Join(view.RootPath, "a.txt"), []byte("patched\n"), 0o644)
	}}
	supervisor, err := NewSupervisor(Config{
		Agents: store, Tasks: store, Mailbox: store, Leases: newLeaseStore(t), Profiles: profiles, Runner: runner,
		Workspace: coordinator, MergeExecutor: executor, WorkspaceRoot: main,
		MaxRunningAgents: 3, MaxLiveAgents: 8, MaxDepth: 3, MaxChildren: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Close() })
	team := &snapshotTeam{t: t, ctx: ctx, store: store, root: root, main: main, supervisor: supervisor}
	task := team.create(orchestration.TaskSpec{Title: "patch a", RequiredProfile: "worker"})
	team.ready(task)
	team.waitTask(task.ID, orchestration.TaskCompleted)
	if mode := seen.get("mode"); mode != string(workspacecoord.ViewGitWorktree) {
		t.Fatalf("worker view mode = %q, want git worktree", mode)
	}
	if content := readTeamFile(t, filepath.Join(main, "a.txt")); content != "patched\n" {
		t.Fatalf("main = %q, want the worktree patch", content)
	}
	worker := team.worker(task.ID)
	if worker.Result == nil || worker.Result.Delivery == nil || worker.Result.Delivery.ChangedPaths != 1 {
		t.Fatalf("worker result has no patch delivery: %#v", worker.Result)
	}
	merge, err := store.GetMerge(ctx, worker.Result.Delivery.MergeID)
	if err != nil {
		t.Fatal(err)
	}
	if merge.Kind != workspacecoord.MergeGitPatch || merge.State != workspacecoord.MergeApplied || merge.TaskID != task.ID {
		t.Fatalf("merge = %#v", merge)
	}
}

func TestWorktreeContinuationCannotMutatePendingReviewView(t *testing.T) {
	ctx := context.Background()
	supervisor, store, root := newSupervisorTest(t, nil, 8, 3, 8)
	child, err := supervisor.Spawn(ctx, childSpec(root, "worktree-owner", "produce patch"))
	if err != nil {
		t.Fatal(err)
	}
	runID, err := orchestration.NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	child, err = store.TransitionAgent(ctx, orchestration.AgentTransition{AgentID: child.ID, Expected: child.Version, Target: orchestration.AgentRunning, RunID: runID})
	if err != nil {
		t.Fatal(err)
	}
	child, err = store.TransitionAgent(ctx, orchestration.AgentTransition{AgentID: child.ID, Expected: child.Version, Target: orchestration.AgentCompleted, Result: &orchestration.AgentResult{Status: orchestration.ResultSucceeded, Summary: "patch ready"}})
	if err != nil {
		t.Fatal(err)
	}
	main := t.TempDir()
	view, err := store.CreateView(ctx, workspacecoord.ViewSpec{TeamID: root.TeamID, AgentID: child.ID, Mode: workspacecoord.ViewGitWorktree, WorkspaceRoot: main, RootPath: main, BaseRevision: "base", CorrelationID: string(runID)})
	if err != nil {
		t.Fatal(err)
	}
	merge, err := store.EnqueueMerge(ctx, workspacecoord.MergeSpec{TeamID: root.TeamID, AgentID: child.ID, ViewID: view.ID, BaseRevision: view.BaseRevision, PatchArtifact: "immutable-patch"})
	if err != nil {
		t.Fatal(err)
	}
	supervisor.workspace = &workspacecoord.Coordinator{Repository: store}
	for _, state := range []workspacecoord.MergeState{workspacecoord.MergePending, workspacecoord.MergeRunning} {
		if state == workspacecoord.MergeRunning {
			merge, err = store.TransitionMerge(ctx, workspacecoord.MergeTransition{MergeID: merge.ID, Expected: merge.Version, Target: state})
			if err != nil {
				t.Fatal(err)
			}
		}
		if _, err := supervisor.Continue(ctx, ContinueCommand{AgentID: child.ID, Prompt: "change reviewed file", IdempotencyKey: "forbidden-next-run"}); !errors.Is(err, orchestration.ErrInvalidEvent) || !strings.Contains(err.Error(), "frozen") {
			t.Fatalf("continuation while merge %s = %v", state, err)
		}
		stored, err := store.GetAgent(ctx, child.ID)
		if err != nil || stored.Version != child.Version || stored.State != orchestration.AgentCompleted {
			t.Fatalf("rejected continuation changed run identity: %#v err=%v", stored, err)
		}
		messages, err := store.ListMessages(ctx, orchestration.MessageQuery{TeamID: root.TeamID, RecipientAgentID: child.ID})
		if err != nil || len(messages) != 0 {
			t.Fatalf("rejected continuation enqueued control input: %#v err=%v", messages, err)
		}
	}
	if _, err := store.TransitionMerge(ctx, workspacecoord.MergeTransition{MergeID: merge.ID, Expected: merge.Version, Target: workspacecoord.MergeRejected}); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Continue(ctx, ContinueCommand{AgentID: child.ID, Prompt: "revise after discard", IdempotencyKey: "allowed-next-run"}); err != nil {
		t.Fatalf("discarded patch kept its worktree frozen: %v", err)
	}
}

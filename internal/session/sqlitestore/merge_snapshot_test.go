package sqlitestore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/artifact"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

// snapshotMergeFixture runs snapshot merges against a real store, a main
// workspace and an artifact store.
type snapshotMergeFixture struct {
	t           *testing.T
	ctx         context.Context
	store       *OrchestrationStore
	root        orchestration.RootThread
	main        string
	artifacts   *artifact.Store
	coordinator *workspacecoord.Coordinator
	executor    *workspacecoord.MergeExecutor
}

// snapshotWork is one worker's task, snapshot view and merge.
type snapshotWork struct {
	agent orchestration.AgentID
	task  orchestration.TaskSnapshot
	view  workspacecoord.View
	merge workspacecoord.MergeRequest
}

// newSnapshotMergeFixture creates a main workspace holding files.
func newSnapshotMergeFixture(t *testing.T, files map[string]string) *snapshotMergeFixture {
	t.Helper()
	ctx := context.Background()
	database, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "snapshot-executor-root"})
	if err != nil {
		t.Fatal(err)
	}
	main, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		writeFixtureFile(t, filepath.Join(main, name), content)
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
	return &snapshotMergeFixture{t: t, ctx: ctx, store: store, root: root, main: main, artifacts: artifacts, coordinator: coordinator, executor: executor}
}

// start creates a worker with an in-progress task and its snapshot view.
// prepare may seed the work tree, like a resolution view.
func (fixture *snapshotMergeFixture) start(name string, parent orchestration.TaskID, prepare func(string, workspacecoord.Manifest) error) *snapshotWork {
	t, ctx := fixture.t, fixture.ctx
	t.Helper()
	worker, err := fixture.store.CreateChild(ctx, orchestration.SpawnSpec{TeamID: fixture.root.TeamID, ParentAgentID: fixture.root.AgentID, ProfileRevision: "worker-v1"})
	if err != nil {
		t.Fatal(err)
	}
	var task orchestration.TaskSnapshot
	if parent != "" {
		task, err = fixture.store.GetTask(ctx, parent)
	} else {
		task, err = fixture.store.CreateTask(ctx, orchestration.TaskSpec{TeamID: fixture.root.TeamID, Title: name})
	}
	if err != nil {
		t.Fatal(err)
	}
	if task.State != orchestration.TaskReady {
		if task, err = fixture.store.TransitionTask(ctx, orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskReady}); err != nil {
			t.Fatal(err)
		}
	}
	if task, err = fixture.store.ClaimTask(ctx, task.ID, worker.ID, task.Version); err != nil {
		t.Fatal(err)
	}
	if task, err = fixture.store.TransitionTask(ctx, orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskInProgress, AssigneeAgentID: worker.ID, ActorAgentID: worker.ID}); err != nil {
		t.Fatal(err)
	}
	view, err := fixture.coordinator.CreateSnapshotView(ctx, workspacecoord.ViewSpec{
		TeamID: fixture.root.TeamID, AgentID: worker.ID, TaskID: task.ID, IdempotencyKey: "view:" + name, CorrelationID: "run-" + name,
	}, workspacecoord.SnapshotRequest{TeamID: fixture.root.TeamID, AgentID: worker.ID, RunID: orchestration.RunID("run-" + name), Prepare: prepare})
	if err != nil {
		t.Fatal(err)
	}
	return &snapshotWork{agent: worker.ID, task: task, view: view}
}

// submit freezes the work tree, queues its merge and moves the task to
// awaiting merge.
func (fixture *snapshotMergeFixture) submit(work *snapshotWork, supersedes orchestration.MergeID) {
	fixture.t.Helper()
	fixture.enqueue(work, supersedes)
	fixture.await(work)
}

// enqueue freezes the work tree and queues its merge.
func (fixture *snapshotMergeFixture) enqueue(work *snapshotWork, supersedes orchestration.MergeID) {
	t, ctx := fixture.t, fixture.ctx
	t.Helper()
	snapshot, err := fixture.coordinator.Snapshots.ForView(work.view)
	if err != nil {
		t.Fatal(err)
	}
	candidate, changed, err := workspacecoord.BuildCandidate(ctx, snapshot, work.view.ID, fixture.artifacts)
	if err != nil || !changed {
		t.Fatalf("build candidate: changed=%v err=%v", changed, err)
	}
	reference, err := workspacecoord.StoreCandidate(ctx, fixture.artifacts, candidate)
	if err != nil {
		t.Fatal(err)
	}
	work.merge, err = fixture.store.EnqueueMerge(ctx, workspacecoord.MergeSpec{
		TeamID: fixture.root.TeamID, AgentID: work.agent, TaskID: work.task.ID, ViewID: work.view.ID,
		BaseRevision: work.view.BaseRevision, Kind: workspacecoord.MergeSnapshot, CandidateDigest: candidate.Digest,
		PatchArtifact: reference, Supersedes: supersedes, IdempotencyKey: "merge:" + string(work.view.ID) + ":" + candidate.Digest,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// await moves a task with a queued merge to awaiting merge.
func (fixture *snapshotMergeFixture) await(work *snapshotWork) {
	t, ctx := fixture.t, fixture.ctx
	t.Helper()
	var err error
	if work.task, err = fixture.store.TransitionTask(ctx, orchestration.TaskTransition{TaskID: work.task.ID, Expected: work.task.Version, Target: orchestration.TaskAwaitingMerge, ActorAgentID: work.agent}); err != nil {
		t.Fatal(err)
	}
}

// run drains the merge queue once.
func (fixture *snapshotMergeFixture) run() []workspacecoord.MergeRequest {
	fixture.t.Helper()
	merges, err := fixture.executor.RunPending(fixture.ctx, fixture.root.TeamID, 0)
	if err != nil {
		fixture.t.Fatal(err)
	}
	return merges
}

// expectMerge checks the stored state of a merge.
func (fixture *snapshotMergeFixture) expectMerge(id orchestration.MergeID, state workspacecoord.MergeState) workspacecoord.MergeRequest {
	fixture.t.Helper()
	merge, err := fixture.store.GetMerge(fixture.ctx, id)
	if err != nil || merge.State != state {
		fixture.t.Fatalf("merge %s = %s (%s), want %s; err=%v", id, merge.State, merge.Reason, state, err)
	}
	return merge
}

// expectTask checks the stored state of a task.
func (fixture *snapshotMergeFixture) expectTask(id orchestration.TaskID, state orchestration.TaskState) {
	fixture.t.Helper()
	task, err := fixture.store.GetTask(fixture.ctx, id)
	if err != nil || task.State != state {
		fixture.t.Fatalf("task %s = %s, want %s; err=%v", id, task.State, state, err)
	}
}

// expectView checks the stored view state and whether its snapshot exists.
func (fixture *snapshotMergeFixture) expectView(id orchestration.WorkspaceViewID, state workspacecoord.ViewState, onDisk bool) {
	fixture.t.Helper()
	view, err := fixture.store.GetView(fixture.ctx, id)
	if err != nil || view.State != state {
		fixture.t.Fatalf("view %s = %s, want %s; err=%v", id, view.State, state, err)
	}
	if _, err := os.Stat(view.RootPath); (err == nil) != onDisk {
		fixture.t.Fatalf("snapshot of view %s on disk = %v, want %v", id, err == nil, onDisk)
	}
}

// writeFixtureFile writes content, creating parents.
func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// readFixtureFile reads a file as text.
func readFixtureFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSnapshotMergesOfParallelWorkersBothReachMain(t *testing.T) {
	fixture := newSnapshotMergeFixture(t, map[string]string{"a.txt": "one\ntwo\nthree\nfour\nfive\n"})
	fixture.executor.AutoMerge = true
	first := fixture.start("first", "", nil)
	second := fixture.start("second", "", nil)
	writeFixtureFile(t, filepath.Join(first.view.RootPath, "a.txt"), "ONE\ntwo\nthree\nfour\nfive\n")
	writeFixtureFile(t, filepath.Join(second.view.RootPath, "a.txt"), "one\ntwo\nthree\nfour\nFIVE\n")
	writeFixtureFile(t, filepath.Join(second.view.RootPath, "b.txt"), "new\n")
	if content := readFixtureFile(t, filepath.Join(fixture.main, "a.txt")); content != "one\ntwo\nthree\nfour\nfive\n" {
		t.Fatalf("main changed before merge: %q", content)
	}
	fixture.submit(first, "")
	fixture.submit(second, "")
	fixture.run()
	if content := readFixtureFile(t, filepath.Join(fixture.main, "a.txt")); content != "ONE\ntwo\nthree\nfour\nFIVE\n" {
		t.Fatalf("merged main = %q", content)
	}
	if content := readFixtureFile(t, filepath.Join(fixture.main, "b.txt")); content != "new\n" {
		t.Fatalf("added file = %q", content)
	}
	for _, work := range []*snapshotWork{first, second} {
		merge := fixture.expectMerge(work.merge.ID, workspacecoord.MergeApplied)
		if merge.PlanDigest == "" {
			t.Fatalf("applied merge %s has no plan digest", merge.ID)
		}
		fixture.expectTask(work.task.ID, orchestration.TaskCompleted)
		fixture.expectView(work.view.ID, workspacecoord.ViewReleased, false)
	}
}

func TestSnapshotMergeWaitsForItsTask(t *testing.T) {
	fixture := newSnapshotMergeFixture(t, map[string]string{"a.txt": "base\n"})
	fixture.executor.AutoMerge = true
	work := fixture.start("waiting", "", nil)
	writeFixtureFile(t, filepath.Join(work.view.RootPath, "a.txt"), "changed\n")
	fixture.enqueue(work, "")
	// The task is still in progress, so the merge stays queued.
	fixture.run()
	fixture.expectMerge(work.merge.ID, workspacecoord.MergePending)
	if content := readFixtureFile(t, filepath.Join(fixture.main, "a.txt")); content != "base\n" {
		t.Fatalf("queued merge changed main: %q", content)
	}
	fixture.await(work)
	fixture.run()
	fixture.expectMerge(work.merge.ID, workspacecoord.MergeApplied)
	fixture.expectTask(work.task.ID, orchestration.TaskCompleted)
}

func TestSnapshotMergeIsRejectedWhenItsTaskMovedOn(t *testing.T) {
	fixture := newSnapshotMergeFixture(t, map[string]string{"a.txt": "base\n"})
	fixture.executor.AutoMerge = true
	work := fixture.start("canceled", "", nil)
	writeFixtureFile(t, filepath.Join(work.view.RootPath, "a.txt"), "changed\n")
	fixture.submit(work, "")
	task, err := fixture.store.GetTask(fixture.ctx, work.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.TransitionTask(fixture.ctx, orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskCanceled, ActorAgentID: work.agent}); err != nil {
		t.Fatal(err)
	}
	fixture.run()
	fixture.expectMerge(work.merge.ID, workspacecoord.MergeRejected)
	if content := readFixtureFile(t, filepath.Join(fixture.main, "a.txt")); content != "base\n" {
		t.Fatalf("rejected merge changed main: %q", content)
	}
	fixture.expectView(work.view.ID, workspacecoord.ViewActive, true)
}

func TestSnapshotMergeApprovalBindsToThePlan(t *testing.T) {
	fixture := newSnapshotMergeFixture(t, map[string]string{"a.txt": "base\n"})
	var asked []workspacecoord.MergeApproval
	fixture.executor.Approver = workspacecoord.MergeApproverFunc(func(_ context.Context, request workspacecoord.MergeApproval) (bool, error) {
		asked = append(asked, request)
		return false, nil
	})
	work := fixture.start("denied", "", nil)
	writeFixtureFile(t, filepath.Join(work.view.RootPath, "a.txt"), "changed\n")
	fixture.submit(work, "")
	fixture.run()
	if len(asked) != 1 || asked[0].Plan.Digest == "" || !strings.Contains(asked[0].Diff, "+changed") {
		t.Fatalf("approval requests = %#v", asked)
	}
	fixture.expectMerge(work.merge.ID, workspacecoord.MergeRejected)
	fixture.expectTask(work.task.ID, orchestration.TaskFailed)
	if content := readFixtureFile(t, filepath.Join(fixture.main, "a.txt")); content != "base\n" {
		t.Fatalf("denied merge changed main: %q", content)
	}
	// The refused work stays on disk for review.
	fixture.expectView(work.view.ID, workspacecoord.ViewActive, true)
}

func TestSnapshotMergeWaitsForAnExecutorThatCanDecide(t *testing.T) {
	fixture := newSnapshotMergeFixture(t, map[string]string{"a.txt": "base\n"})
	work := fixture.start("undecided", "", nil)
	writeFixtureFile(t, filepath.Join(work.view.RootPath, "a.txt"), "changed\n")
	fixture.submit(work, "")
	fixture.run()
	fixture.expectMerge(work.merge.ID, workspacecoord.MergePending)
	fixture.expectTask(work.task.ID, orchestration.TaskAwaitingMerge)
	fixture.executor.Approver = workspacecoord.MergeApproverFunc(func(context.Context, workspacecoord.MergeApproval) (bool, error) {
		return true, nil
	})
	fixture.run()
	fixture.expectMerge(work.merge.ID, workspacecoord.MergeApplied)
	if content := readFixtureFile(t, filepath.Join(fixture.main, "a.txt")); content != "changed\n" {
		t.Fatalf("approved merge left main at %q", content)
	}
}

func TestSnapshotConflictIsResolvedBySupersedingMerge(t *testing.T) {
	fixture := newSnapshotMergeFixture(t, map[string]string{"a.txt": "one\ntwo\nthree\n"})
	fixture.executor.AutoMerge = true
	first := fixture.start("first", "", nil)
	second := fixture.start("second", "", nil)
	writeFixtureFile(t, filepath.Join(first.view.RootPath, "a.txt"), "one\nTWO-first\nthree\n")
	writeFixtureFile(t, filepath.Join(second.view.RootPath, "a.txt"), "one\nTWO-second\nthree\n")
	fixture.submit(first, "")
	fixture.submit(second, "")
	fixture.run()
	fixture.expectMerge(first.merge.ID, workspacecoord.MergeApplied)
	conflicted := fixture.expectMerge(second.merge.ID, workspacecoord.MergeConflicted)
	if conflicted.ResolutionTaskID == "" || !strings.Contains(conflicted.Reason, "a.txt") {
		t.Fatalf("conflicted merge = %#v", conflicted)
	}
	fixture.expectTask(second.task.ID, orchestration.TaskAwaitingMerge)
	// The conflicted snapshot stays until a resolution supersedes it.
	fixture.expectView(second.view.ID, workspacecoord.ViewActive, true)

	candidate, err := workspacecoord.LoadCandidate(fixture.ctx, fixture.artifacts, conflicted.PatchArtifact)
	if err != nil {
		t.Fatal(err)
	}
	resolver := fixture.start("resolver", conflicted.ResolutionTaskID, workspacecoord.PrepareResolution(fixture.ctx, candidate, fixture.artifacts))
	seeded := readFixtureFile(t, filepath.Join(resolver.view.RootPath, "a.txt"))
	if !workspacecoord.HasConflictMarkers(seeded) || !strings.Contains(seeded, "TWO-first") || !strings.Contains(seeded, "TWO-second") {
		t.Fatalf("resolution work tree = %q", seeded)
	}
	writeFixtureFile(t, filepath.Join(resolver.view.RootPath, "a.txt"), "one\nTWO-both\nthree\n")
	fixture.submit(resolver, conflicted.ID)
	fixture.expectMerge(second.merge.ID, workspacecoord.MergeSuperseded)
	fixture.run()
	fixture.expectMerge(resolver.merge.ID, workspacecoord.MergeApplied)
	if content := readFixtureFile(t, filepath.Join(fixture.main, "a.txt")); content != "one\nTWO-both\nthree\n" {
		t.Fatalf("resolved main = %q", content)
	}
	fixture.expectTask(resolver.task.ID, orchestration.TaskCompleted)
	fixture.expectTask(second.task.ID, orchestration.TaskCompleted)
	fixture.expectView(second.view.ID, workspacecoord.ViewReleased, false)
	fixture.expectView(resolver.view.ID, workspacecoord.ViewReleased, false)
}

func TestSnapshotMergeResumesJournaledCommit(t *testing.T) {
	fixture := newSnapshotMergeFixture(t, map[string]string{"a.txt": "base\n", "b.txt": "base\n"})
	fixture.executor.AutoMerge = true
	work := fixture.start("resume", "", nil)
	writeFixtureFile(t, filepath.Join(work.view.RootPath, "a.txt"), "changed a\n")
	writeFixtureFile(t, filepath.Join(work.view.RootPath, "b.txt"), "changed b\n")
	fixture.submit(work, "")
	// Model a crash after the first path of an approved plan reached main.
	running, err := fixture.store.TransitionMerge(fixture.ctx, workspacecoord.MergeTransition{MergeID: work.merge.ID, Expected: work.merge.Version, Target: workspacecoord.MergeRunning, ActorAgentID: work.agent})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := workspacecoord.LoadCandidate(fixture.ctx, fixture.artifacts, running.PatchArtifact)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := workspacecoord.BuildApplyPlan(fixture.ctx, workspacecoord.PlanInput{MergeID: running.ID, Candidate: candidate, WorkspaceRoot: fixture.main, Artifacts: fixture.artifacts})
	if err != nil || len(plan.Entries) != 2 {
		t.Fatalf("plan = %#v err=%v", plan, err)
	}
	if _, err := fixture.store.BeginApply(fixture.ctx, plan); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(fixture.main, "a.txt"), "changed a\n")
	if err := fixture.store.MarkPathApplied(fixture.ctx, running.ID, "a.txt"); err != nil {
		t.Fatal(err)
	}
	fixture.run()
	applied := fixture.expectMerge(work.merge.ID, workspacecoord.MergeApplied)
	if applied.PlanDigest != plan.Digest {
		t.Fatalf("resumed plan digest = %s, want %s", applied.PlanDigest, plan.Digest)
	}
	if content := readFixtureFile(t, filepath.Join(fixture.main, "b.txt")); content != "changed b\n" {
		t.Fatalf("resumed main = %q", content)
	}
	fixture.expectTask(work.task.ID, orchestration.TaskCompleted)
}

func TestSnapshotViewWithoutChangesIsDiscarded(t *testing.T) {
	fixture := newSnapshotMergeFixture(t, map[string]string{"a.txt": "base\n"})
	work := fixture.start("idle", "", nil)
	if err := fixture.coordinator.FinalizeView(fixture.ctx, work.view.ID, "task completed"); err != nil {
		t.Fatal(err)
	}
	fixture.expectView(work.view.ID, workspacecoord.ViewReleased, false)
	changed := fixture.start("unmerged", "", nil)
	writeFixtureFile(t, filepath.Join(changed.view.RootPath, "a.txt"), "changed\n")
	if err := fixture.coordinator.FinalizeView(fixture.ctx, changed.view.ID, "task failed"); err != nil {
		t.Fatal(err)
	}
	fixture.expectView(changed.view.ID, workspacecoord.ViewActive, true)
}

func TestDiscardedSnapshotMergeFailsItsTaskAndKeepsTheCandidate(t *testing.T) {
	fixture := newSnapshotMergeFixture(t, map[string]string{"a.txt": "base\n"})
	work := fixture.start("discarded", "", nil)
	writeFixtureFile(t, filepath.Join(work.view.RootPath, "a.txt"), "changed\n")
	fixture.submit(work, "")
	// No approver can decide, so the merge waits for the user.
	fixture.run()
	fixture.expectMerge(work.merge.ID, workspacecoord.MergePending)
	discarded, err := fixture.executor.Discard(fixture.ctx, work.merge.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if discarded.State != workspacecoord.MergeRejected || discarded.Reason != "candidate discarded by the user" {
		t.Fatalf("discarded merge = %s (%s)", discarded.State, discarded.Reason)
	}
	fixture.expectTask(work.task.ID, orchestration.TaskFailed)
	fixture.expectView(work.view.ID, workspacecoord.ViewActive, true)
	if content := readFixtureFile(t, filepath.Join(fixture.main, "a.txt")); content != "base\n" {
		t.Fatalf("discarded merge changed main: %q", content)
	}
	if _, err := fixture.executor.Discard(fixture.ctx, work.merge.ID, ""); err == nil {
		t.Fatal("a rejected merge was discarded twice")
	}
}

package workspacecoord_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/artifact"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

// snapshotFixture owns a main workspace, a views directory and an artifact
// store for snapshot tests.
type snapshotFixture struct {
	t         *testing.T
	main      string
	store     *workspacecoord.SnapshotStore
	artifacts *artifact.Store
}

// newSnapshotFixture creates a workspace holding files.
func newSnapshotFixture(t *testing.T, files map[string]string) *snapshotFixture {
	t.Helper()
	main, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	artifacts, err := artifact.NewStore(filepath.Join(data, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	fixture := &snapshotFixture{
		t: t, main: main, artifacts: artifacts,
		store: &workspacecoord.SnapshotStore{ViewsDirectory: filepath.Join(data, "views"), WorkspaceRoot: main},
	}
	for path, content := range files {
		writeTestFile(t, filepath.Join(main, path), content)
	}
	return fixture
}

// snapshot materializes the snapshot of one run.
func (fixture *snapshotFixture) snapshot(agent string) workspacecoord.Snapshot {
	fixture.t.Helper()
	snapshot, err := fixture.store.Materialize(context.Background(), workspacecoord.SnapshotRequest{
		TeamID: "team-1", AgentID: orchestration.AgentID(agent), RunID: "run-1",
	})
	if err != nil {
		fixture.t.Fatal(err)
	}
	return snapshot
}

// candidate freezes a snapshot's changes.
func (fixture *snapshotFixture) candidate(snapshot workspacecoord.Snapshot) workspacecoord.Candidate {
	fixture.t.Helper()
	candidate, changed, err := workspacecoord.BuildCandidate(context.Background(), snapshot, "view-1", fixture.artifacts)
	if err != nil || !changed {
		fixture.t.Fatalf("candidate changed=%v err=%v", changed, err)
	}
	return candidate
}

// plan builds the apply plan of a candidate against main.
func (fixture *snapshotFixture) plan(mergeID string, candidate workspacecoord.Candidate) workspacecoord.ApplyPlan {
	fixture.t.Helper()
	plan, err := workspacecoord.BuildApplyPlan(context.Background(), workspacecoord.PlanInput{
		MergeID: orchestration.MergeID(mergeID), Candidate: candidate, WorkspaceRoot: fixture.main, Artifacts: fixture.artifacts,
	})
	if err != nil {
		fixture.t.Fatal(err)
	}
	return plan
}

// writeTestFile writes content and creates parents.
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// readTestFile returns a file's content.
func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSnapshotMaterializeIsIsolatedAndReused(t *testing.T) {
	fixture := newSnapshotFixture(t, map[string]string{"a.txt": "one\n", "dir/b.txt": "two\n", ".git/HEAD": "ref\n"})
	if err := os.Symlink("a.txt", filepath.Join(fixture.main, "link")); err != nil {
		t.Fatal(err)
	}
	snapshot := fixture.snapshot("agent-1")
	if !strings.HasPrefix(snapshot.Digest, "sha256:") || len(snapshot.Manifest.Entries) != 3 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if target, err := os.Readlink(filepath.Join(snapshot.WorkPath, "link")); err != nil || target != "a.txt" {
		t.Fatalf("symlink target = %q, %v", target, err)
	}
	writeTestFile(t, filepath.Join(snapshot.WorkPath, "a.txt"), "changed\n")
	if got := readTestFile(t, filepath.Join(fixture.main, "a.txt")); got != "one\n" {
		t.Fatalf("main changed through snapshot: %q", got)
	}
	if got := readTestFile(t, filepath.Join(snapshot.BasePath, "a.txt")); got != "one\n" {
		t.Fatalf("baseline changed through work tree: %q", got)
	}
	again := fixture.snapshot("agent-1")
	if again.Root != snapshot.Root || again.Digest != snapshot.Digest {
		t.Fatalf("retry did not reuse snapshot: %+v", again)
	}
}

func TestSnapshotCopyLimitFailsClosed(t *testing.T) {
	fixture := newSnapshotFixture(t, map[string]string{"big.txt": strings.Repeat("x", 4096)})
	workspacecoord.DisableCloneForTest(t)
	fixture.store.CopyLimit = 1
	_, err := fixture.store.Materialize(context.Background(), workspacecoord.SnapshotRequest{TeamID: "team-1", AgentID: "agent-1", RunID: "run-1"})
	if !errors.Is(err, workspacecoord.ErrSnapshotCopyLimit) {
		t.Fatalf("err = %v", err)
	}
	entries, _ := os.ReadDir(fixture.store.ViewsDirectory)
	if len(entries) != 0 {
		t.Fatalf("failed snapshot left %d entries", len(entries))
	}
}

func TestSnapshotPlainCopyWithinLimit(t *testing.T) {
	workspacecoord.DisableCloneForTest(t)
	fixture := newSnapshotFixture(t, map[string]string{"a.txt": "one\n", ".git/HEAD": "ref\n"})
	snapshot := fixture.snapshot("agent-1")
	if snapshot.Cloned {
		t.Fatal("plain copy reported a clone")
	}
	if _, err := os.Stat(filepath.Join(snapshot.WorkPath, ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plain copy carried .git: %v", err)
	}
}

func TestBuildCandidateDetectsNoChange(t *testing.T) {
	fixture := newSnapshotFixture(t, map[string]string{"a.txt": "one\n"})
	snapshot := fixture.snapshot("agent-1")
	_, changed, err := workspacecoord.BuildCandidate(context.Background(), snapshot, "view-1", fixture.artifacts)
	if err != nil || changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
}
func TestTwoSnapshotsEditingOneFileMergeAutomatically(t *testing.T) {
	base := "header\nalpha\nmiddle\nomega\nfooter\n"
	fixture := newSnapshotFixture(t, map[string]string{"shared.txt": base})
	first, second := fixture.snapshot("agent-1"), fixture.snapshot("agent-2")
	writeTestFile(t, filepath.Join(first.WorkPath, "shared.txt"), strings.Replace(base, "alpha", "ALPHA", 1))
	writeTestFile(t, filepath.Join(second.WorkPath, "shared.txt"), strings.Replace(base, "omega", "OMEGA", 1))
	writeTestFile(t, filepath.Join(second.WorkPath, "new/added.txt"), "added\n")
	for index, snapshot := range []workspacecoord.Snapshot{first, second} {
		plan := fixture.plan("merge-"+string(rune('a'+index)), fixture.candidate(snapshot))
		if len(plan.Conflicts) != 0 {
			t.Fatalf("plan %d conflicts: %+v", index, plan.Conflicts)
		}
		if err := workspacecoord.CommitPlan(context.Background(), fixture.main, plan, fixture.artifacts, workspacecoord.NewMemoryApplyJournal(), nil); err != nil {
			t.Fatal(err)
		}
	}
	want := "header\nALPHA\nmiddle\nOMEGA\nfooter\n"
	if got := readTestFile(t, filepath.Join(fixture.main, "shared.txt")); got != want {
		t.Fatalf("main = %q", got)
	}
	if got := readTestFile(t, filepath.Join(fixture.main, "new/added.txt")); got != "added\n" {
		t.Fatalf("added = %q", got)
	}
}

func TestSameRegionEditsConflictWithoutTouchingMain(t *testing.T) {
	fixture := newSnapshotFixture(t, map[string]string{"shared.txt": "a\nb\nc\n"})
	snapshot := fixture.snapshot("agent-1")
	writeTestFile(t, filepath.Join(snapshot.WorkPath, "shared.txt"), "a\ncandidate\nc\n")
	writeTestFile(t, filepath.Join(fixture.main, "shared.txt"), "a\nmain\nc\n")
	plan := fixture.plan("merge-1", fixture.candidate(snapshot))
	if len(plan.Conflicts) != 1 || plan.Conflicts[0].Path != "shared.txt" {
		t.Fatalf("conflicts = %+v", plan.Conflicts)
	}
	if got := readTestFile(t, filepath.Join(fixture.main, "shared.txt")); got != "a\nmain\nc\n" {
		t.Fatalf("main changed: %q", got)
	}
}

func TestApplyPlanRejectsConflictMarkers(t *testing.T) {
	fixture := newSnapshotFixture(t, map[string]string{"a.txt": "a\n"})
	snapshot := fixture.snapshot("agent-1")
	writeTestFile(t, filepath.Join(snapshot.WorkPath, "a.txt"), "<<<<<<< gsea:main\nx\n=======\ny\n>>>>>>> gsea:candidate m\n")
	plan := fixture.plan("merge-1", fixture.candidate(snapshot))
	if len(plan.Conflicts) != 1 || len(plan.Entries) != 0 {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestApplyPlanDigestChangesWithMain(t *testing.T) {
	fixture := newSnapshotFixture(t, map[string]string{"a.txt": "a\nb\nc\n"})
	snapshot := fixture.snapshot("agent-1")
	writeTestFile(t, filepath.Join(snapshot.WorkPath, "a.txt"), "a\nb\nC\n")
	candidate := fixture.candidate(snapshot)
	first := fixture.plan("merge-1", candidate)
	writeTestFile(t, filepath.Join(fixture.main, "a.txt"), "A\nb\nc\n")
	second := fixture.plan("merge-1", candidate)
	if first.Digest == second.Digest {
		t.Fatal("plan digest ignored a main change")
	}
	if err := workspacecoord.VerifyPreimages(fixture.main, first); !errors.Is(err, workspacecoord.ErrApplyDiverged) {
		t.Fatalf("stale plan verified: %v", err)
	}
}

// errCrash simulates a process crash at a commit step.
var errCrash = errors.New("injected crash")

func TestCommitRecoversFromCrashAtEveryStep(t *testing.T) {
	steps := []string{workspacecoord.StepJournaled, workspacecoord.StepWritten, workspacecoord.StepRecorded, workspacecoord.StepVerified}
	for _, step := range steps {
		for _, crashPath := range []string{"a.txt", "b.txt", ""} {
			t.Run(step+"/"+crashPath, func(t *testing.T) {
				fixture := newSnapshotFixture(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n", "gone.txt": "x\n"})
				snapshot := fixture.snapshot("agent-1")
				writeTestFile(t, filepath.Join(snapshot.WorkPath, "a.txt"), "A\n")
				writeTestFile(t, filepath.Join(snapshot.WorkPath, "b.txt"), "B\n")
				if err := os.Remove(filepath.Join(snapshot.WorkPath, "gone.txt")); err != nil {
					t.Fatal(err)
				}
				plan := fixture.plan("merge-1", fixture.candidate(snapshot))
				journal := workspacecoord.NewMemoryApplyJournal()
				crashed := false
				hook := func(current, path string) error {
					if current == step && (path == crashPath || crashPath == "") && !crashed {
						crashed = true
						return errCrash
					}
					return nil
				}
				err := workspacecoord.CommitPlan(context.Background(), fixture.main, plan, fixture.artifacts, journal, hook)
				if crashed && !errors.Is(err, errCrash) {
					t.Fatalf("crash was not reported: %v", err)
				}
				if err := workspacecoord.CommitPlan(context.Background(), fixture.main, plan, fixture.artifacts, journal, nil); err != nil {
					t.Fatalf("recovery: %v", err)
				}
				record, ok, _ := journal.LoadApply(context.Background(), "merge-1")
				if !ok || record.State != workspacecoord.ApplyFinished {
					t.Fatalf("record = %+v", record)
				}
				if readTestFile(t, filepath.Join(fixture.main, "a.txt")) != "A\n" || readTestFile(t, filepath.Join(fixture.main, "b.txt")) != "B\n" {
					t.Fatal("recovery did not reach the postimage")
				}
				if _, err := os.Stat(filepath.Join(fixture.main, "gone.txt")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("deleted path remains: %v", err)
				}
			})
		}
	}
}

func TestCommitStopsWhenMainDivergesDuringRecovery(t *testing.T) {
	fixture := newSnapshotFixture(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n"})
	snapshot := fixture.snapshot("agent-1")
	writeTestFile(t, filepath.Join(snapshot.WorkPath, "a.txt"), "A\n")
	writeTestFile(t, filepath.Join(snapshot.WorkPath, "b.txt"), "B\n")
	plan := fixture.plan("merge-1", fixture.candidate(snapshot))
	journal := workspacecoord.NewMemoryApplyJournal()
	hook := func(step, path string) error {
		if step == workspacecoord.StepRecorded && path == "a.txt" {
			return errCrash
		}
		return nil
	}
	_ = workspacecoord.CommitPlan(context.Background(), fixture.main, plan, fixture.artifacts, journal, hook)
	writeTestFile(t, filepath.Join(fixture.main, "b.txt"), "someone else\n")
	err := workspacecoord.CommitPlan(context.Background(), fixture.main, plan, fixture.artifacts, journal, nil)
	if !errors.Is(err, workspacecoord.ErrApplyDiverged) {
		t.Fatalf("err = %v", err)
	}
	if got := readTestFile(t, filepath.Join(fixture.main, "b.txt")); got != "someone else\n" {
		t.Fatalf("diverged path was overwritten: %q", got)
	}
}

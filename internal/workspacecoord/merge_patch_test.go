package workspacecoord_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

// patchMergeFixture queues the worktree patch of view for a task that waits
// for it, like a finished git-worktree run.
func patchMergeFixture(t *testing.T, content string) (*workspacecoord.MergeExecutor, workspacecoord.View, orchestration.TaskSnapshot, workspacecoord.MergeRequest) {
	t.Helper()
	ctx := context.Background()
	coordinator, view := preservationFixture(t)
	tasks := coordinator.Repository.(orchestration.TaskRepository)
	task, err := tasks.CreateTask(ctx, orchestration.TaskSpec{TeamID: view.TeamID, Title: "patch"})
	if err != nil {
		t.Fatal(err)
	}
	if task.State != orchestration.TaskReady {
		if task, err = tasks.TransitionTask(ctx, orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskReady}); err != nil {
			t.Fatal(err)
		}
	}
	if task, err = tasks.ClaimTask(ctx, task.ID, view.AgentID, task.Version); err != nil {
		t.Fatal(err)
	}
	if task, err = tasks.TransitionTask(ctx, orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskInProgress, AssigneeAgentID: view.AgentID, ActorAgentID: view.AgentID}); err != nil {
		t.Fatal(err)
	}
	fixtureWrite(t, filepath.Join(view.RootPath, "tracked.txt"), []byte(content))
	patch, err := coordinator.Worktrees.Diff(ctx, view)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := coordinator.Artifacts.Put(ctx, patch)
	if err != nil {
		t.Fatal(err)
	}
	merge, err := coordinator.Repository.EnqueueMerge(ctx, workspacecoord.MergeSpec{
		TeamID: view.TeamID, AgentID: view.AgentID, TaskID: task.ID, ViewID: view.ID,
		BaseRevision: view.BaseRevision, PatchArtifact: reference, Kind: workspacecoord.MergeGitPatch, IdempotencyKey: "patch-merge",
	})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := workspacecoord.NewMergeExecutor(coordinator.Repository, coordinator, coordinator.Worktrees, coordinator.Artifacts.(workspacecoord.ArtifactReader), tasks)
	if err != nil {
		t.Fatal(err)
	}
	return executor, view, task, merge
}

// awaitPatch moves the task of a queued patch to awaiting merge.
func awaitPatch(t *testing.T, executor *workspacecoord.MergeExecutor, task orchestration.TaskSnapshot) {
	t.Helper()
	current, err := executor.Tasks.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Tasks.TransitionTask(context.Background(), orchestration.TaskTransition{TaskID: task.ID, Expected: current.Version, Target: orchestration.TaskAwaitingMerge, ActorAgentID: task.AssigneeAgentID}); err != nil {
		t.Fatal(err)
	}
}

// expectPatchState checks the stored merge and task states.
func expectPatchState(t *testing.T, executor *workspacecoord.MergeExecutor, merge workspacecoord.MergeRequest, mergeState workspacecoord.MergeState, taskState orchestration.TaskState) {
	t.Helper()
	stored, err := executor.Repository.GetMerge(context.Background(), merge.ID)
	if err != nil || stored.State != mergeState {
		t.Fatalf("merge = %s (%s), want %s; err=%v", stored.State, stored.Reason, mergeState, err)
	}
	task, err := executor.Tasks.GetTask(context.Background(), merge.TaskID)
	if err != nil || task.State != taskState {
		t.Fatalf("task = %s, want %s; err=%v", task.State, taskState, err)
	}
}

// readMain reads tracked.txt of the main workspace.
func readMain(t *testing.T, view workspacecoord.View) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(view.WorkspaceRoot, "tracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestAutomaticPatchMergeWaitsForTaskAndApprover(t *testing.T) {
	executor, view, task, merge := patchMergeFixture(t, "patched\n")
	ctx := context.Background()
	// Without an approver the merge stays queued for a process that can ask.
	awaitPatch(t, executor, task)
	if _, err := executor.RunPendingAutomatic(ctx, view.TeamID, 0); err != nil {
		t.Fatal(err)
	}
	expectPatchState(t, executor, merge, workspacecoord.MergePending, orchestration.TaskAwaitingMerge)
	var asked []workspacecoord.MergeApproval
	executor.Approver = workspacecoord.MergeApproverFunc(func(_ context.Context, request workspacecoord.MergeApproval) (bool, error) {
		asked = append(asked, request)
		return true, nil
	})
	if _, err := executor.RunPendingAutomatic(ctx, view.TeamID, 0); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || !slices.Equal(asked[0].Paths, []string{"tracked.txt"}) || !strings.Contains(asked[0].Diff, "+patched") {
		t.Fatalf("approval requests = %#v", asked)
	}
	expectPatchState(t, executor, merge, workspacecoord.MergeApplied, orchestration.TaskCompleted)
	if content := readMain(t, view); content != "patched\n" {
		t.Fatalf("main = %q", content)
	}
}

func TestExplicitPatchRunDoesNotImplyApproval(t *testing.T) {
	executor, view, task, merge := patchMergeFixture(t, "unapproved\n")
	awaitPatch(t, executor, task)
	if _, err := executor.RunPending(context.Background(), view.TeamID, 0); err != nil {
		t.Fatal(err)
	}
	expectPatchState(t, executor, merge, workspacecoord.MergePending, orchestration.TaskAwaitingMerge)
	if got := readMain(t, view); got != "base\n" {
		t.Fatalf("unapproved explicit run changed main: %q", got)
	}
}

func TestAutomaticPatchMergeWaitsWhileTaskIsInProgress(t *testing.T) {
	executor, view, _, merge := patchMergeFixture(t, "patched\n")
	executor.AutoMerge = true
	if _, err := executor.RunPendingAutomatic(context.Background(), view.TeamID, 0); err != nil {
		t.Fatal(err)
	}
	expectPatchState(t, executor, merge, workspacecoord.MergePending, orchestration.TaskInProgress)
	if content := readMain(t, view); content != "base\n" {
		t.Fatalf("queued patch changed main: %q", content)
	}
}

func TestDeniedPatchMergeFailsItsTaskAndKeepsTheWorktree(t *testing.T) {
	executor, view, task, merge := patchMergeFixture(t, "patched\n")
	executor.Approver = workspacecoord.MergeApproverFunc(func(context.Context, workspacecoord.MergeApproval) (bool, error) {
		return false, nil
	})
	awaitPatch(t, executor, task)
	if _, err := executor.RunPendingAutomatic(context.Background(), view.TeamID, 0); err != nil {
		t.Fatal(err)
	}
	expectPatchState(t, executor, merge, workspacecoord.MergeRejected, orchestration.TaskFailed)
	if content := readMain(t, view); content != "base\n" {
		t.Fatalf("denied patch changed main: %q", content)
	}
	if _, err := os.Stat(filepath.Join(view.RootPath, "tracked.txt")); err != nil {
		t.Fatalf("worktree removed: %v", err)
	}
}

// verifierFunc adapts a function to MergeVerifier.
type verifierFunc func(context.Context, workspacecoord.MergeRequest) error

func (verify verifierFunc) VerifyMerge(ctx context.Context, merge workspacecoord.MergeRequest) error {
	return verify(ctx, merge)
}

func TestPostMergeCheckSettlesTheTaskOnTheMergedMain(t *testing.T) {
	for name, check := range map[string]struct {
		err  error
		want orchestration.TaskState
	}{
		"passed": {want: orchestration.TaskCompleted},
		"failed": {err: errors.New("tests failed"), want: orchestration.TaskFailed},
	} {
		t.Run(name, func(t *testing.T) {
			executor, view, task, merge := patchMergeFixture(t, "patched\n")
			executor.AutoMerge = true
			var checked []string
			executor.Verifier = verifierFunc(func(_ context.Context, applied workspacecoord.MergeRequest) error {
				if applied.ID != merge.ID {
					t.Errorf("verified merge %s, want %s", applied.ID, merge.ID)
				}
				checked = append(checked, readMain(t, view))
				return check.err
			})
			awaitPatch(t, executor, task)
			if _, err := executor.RunPendingAutomatic(context.Background(), view.TeamID, 0); err != nil {
				t.Fatal(err)
			}
			// The check sees the merged main, and a failure keeps the change.
			if !slices.Equal(checked, []string{"patched\n"}) {
				t.Fatalf("checked main = %q", checked)
			}
			expectPatchState(t, executor, merge, workspacecoord.MergeApplied, check.want)
			if content := readMain(t, view); content != "patched\n" {
				t.Fatalf("main = %q", content)
			}
		})
	}
}

func TestPatchPathsListsPatchedFiles(t *testing.T) {
	patch := []byte("diff --git a/one.txt b/one.txt\nindex 1..2\n--- a/one.txt\n+++ b/one.txt\n" +
		"diff --git a/dir/with b/space.txt b/dir/with b/space.txt\nnew file mode 100644\n")
	if paths := workspacecoord.PatchPaths(patch); !slices.Equal(paths, []string{"one.txt", "dir/with b/space.txt"}) {
		t.Fatalf("paths = %q", paths)
	}
}

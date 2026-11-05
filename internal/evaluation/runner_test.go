package evaluation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunnerCreatesPatchRunsHiddenCheckAndRemovesWorktree(t *testing.T) {
	repository, base := newEvaluationRepository(t)
	root := t.TempDir()
	task := Task{
		Schema: TaskSchema, ID: "runner/task-1", Suite: "custom", Kind: "patch", Language: "text",
		Repository: repository, BaseCommit: base, ProblemStatement: "write fixed.txt",
		Checks: []Check{{Name: "hidden-target", Phase: PhaseTarget, Command: "test \"$(cat fixed.txt)\" = fixed"}},
	}
	runner, err := NewRunner(RunnerConfig{
		Root: root, RunID: "run-1", Model: "fake-model",
		Agent: func(_ context.Context, received AgentRequest, workspace, runDirectory string) (AgentOutcome, error) {
			if received.ProblemStatement != "write fixed.txt" || strings.Contains(received.ProblemStatement, "hidden-target") {
				t.Fatalf("Agent received evaluator-only data: %#v", received)
			}
			if err := os.WriteFile(filepath.Join(workspace, "fixed.txt"), []byte("fixed\n"), 0o600); err != nil {
				return AgentOutcome{}, err
			}
			artifact := filepath.Join(workspace, ".gsea", "artifacts", "runtime-owned")
			if err := os.MkdirAll(filepath.Dir(artifact), 0o700); err != nil {
				return AgentOutcome{}, err
			}
			if err := os.WriteFile(artifact, []byte("exclude me"), 0o600); err != nil {
				return AgentOutcome{}, err
			}
			return AgentOutcome{Status: "completed", SessionID: "session-1"}, nil
		},
		Verifier: func(_ context.Context, workspace, runDirectory string, checks []Check) ([]CheckResult, error) {
			content, err := os.ReadFile(filepath.Join(workspace, "fixed.txt"))
			if err != nil {
				return nil, err
			}
			if len(checks) != 1 || checks[0].Name != "hidden-target" || string(content) != "fixed\n" {
				t.Fatalf("checks=%#v content=%q", checks, content)
			}
			return []CheckResult{{Name: checks[0].Name, Phase: PhaseTarget, Backend: BackendSandbox, Passed: true}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := runner.RunTask(context.Background(), task)
	if !result.Resolved || len(result.Checks) != 1 || !result.Checks[0].Passed {
		t.Fatalf("result = %#v", result)
	}
	if !strings.Contains(result.Patch, "fixed.txt") || !strings.Contains(result.Patch, "+fixed") {
		t.Fatalf("patch does not contain new file:\n%s", result.Patch)
	}
	if strings.Contains(result.Patch, "runtime-owned") || strings.Contains(result.Patch, ".gsea/artifacts") {
		t.Fatalf("runtime artifacts leaked into patch:\n%s", result.Patch)
	}
	if result.WorkspacePath != "" {
		t.Fatalf("removed worktree was exposed at %q", result.WorkspacePath)
	}
	worktreeRoot := filepath.Join(root, "worktree-runs", safePathComponent("run-1"), "workspaces", safePathComponent(task.ID))
	if _, err := os.Stat(worktreeRoot); !os.IsNotExist(err) {
		t.Fatalf("worktree still exists: %v", err)
	}
}

func TestPrepareWorktreePatchIncludesTrackedChanges(t *testing.T) {
	repository, base := newEvaluationRepository(t)
	task := Task{
		Schema: TaskSchema, ID: "patch-1", Suite: "custom", Kind: "patch",
		Repository: repository, BaseCommit: base, ProblemStatement: "modify tracked file",
		Checks: []Check{{Name: "noop", Command: "true"}},
	}
	worktree, err := PrepareWorktree(context.Background(), t.TempDir(), task)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = worktree.Remove(context.Background()) })
	if err := os.WriteFile(filepath.Join(worktree.Path, "tracked.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	patch, err := worktree.Patch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(patch), "-base") || !strings.Contains(string(patch), "+changed") {
		t.Fatalf("tracked patch =\n%s", patch)
	}
}

func TestPrepareWorktreeRejectsEvaluationRootInsideSourceRepository(t *testing.T) {
	repository, base := newEvaluationRepository(t)
	task := Task{
		Schema: TaskSchema, ID: "overlap", Suite: "custom", Kind: "patch",
		Repository: repository, BaseCommit: base, ProblemStatement: "do not run",
		Checks: []Check{{Name: "tests", Command: "true"}},
	}
	root := filepath.Join(repository, ".evaluation")
	if _, err := PrepareWorkspace(context.Background(), root, task); err == nil || !strings.Contains(err.Error(), "inside the source repository") {
		t.Fatalf("overlapping root error = %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("overlapping root was created: %v", err)
	}
	runner, err := NewRunner(RunnerConfig{
		Root: root, RunID: "run", Model: "fake",
		Agent: func(context.Context, AgentRequest, string, string) (AgentOutcome, error) {
			t.Fatal("overlapping task must fail before Agent execution")
			return AgentOutcome{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := runner.RunTask(context.Background(), task)
	if result.Error == "" || !strings.Contains(result.Error, "inside the source fixture") {
		t.Fatalf("runner overlap result = %#v", result)
	}
	if _, err := os.Stat(filepath.Join(root, "runs")); !os.IsNotExist(err) {
		t.Fatalf("runner created overlapping artifact directory: %v", err)
	}
}

func TestPrepareDirectoryCopyCapturesChangesWithoutMutatingFixture(t *testing.T) {
	fixture := t.TempDir()
	if err := os.WriteFile(filepath.Join(fixture, ".git"), []byte("gitdir: /must/not/be/used\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, ".gitignore"), []byte("ignored.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "ignored.txt"), []byte("baseline\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	task := Task{
		Schema: TaskSchema, ID: "directory-task", Suite: "terminal", Kind: "terminal",
		WorkspaceMode: WorkspaceDirectory, Repository: fixture, ProblemStatement: "modify the fixture",
		Checks: []Check{{Name: "tests", Command: "true"}},
	}
	workspace, err := PrepareWorkspace(context.Background(), t.TempDir(), task)
	if err != nil {
		t.Fatal(err)
	}
	workspacePath := workspace.Path
	gitMetadata, err := os.Stat(filepath.Join(workspace.Path, ".git"))
	if err != nil || !gitMetadata.IsDir() {
		t.Fatalf("workspace .git is not a private directory: info=%v err=%v", gitMetadata, err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Path, "ignored.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Path, "created.txt"), []byte("created\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	patch, err := workspace.Patch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(patch), "-baseline") || !strings.Contains(string(patch), "+changed") || !strings.Contains(string(patch), "created.txt") {
		t.Fatalf("directory patch =\n%s", patch)
	}
	fixtureContent, err := os.ReadFile(filepath.Join(fixture, "ignored.txt"))
	if err != nil || string(fixtureContent) != "baseline\n" {
		t.Fatalf("fixture content = %q, err=%v", fixtureContent, err)
	}
	if err := workspace.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(workspacePath); !os.IsNotExist(err) {
		t.Fatalf("directory workspace still exists: %v", err)
	}
}

func TestRunnerDoesNotResolveAnAgentFailureEvenWhenChecksPass(t *testing.T) {
	repository, base := newEvaluationRepository(t)
	task := Task{
		Schema: TaskSchema, ID: "failed-agent", Suite: "custom", Kind: "patch",
		Repository: repository, BaseCommit: base, ProblemStatement: "attempt a fix",
		Checks: []Check{{Name: "tests", Command: "true"}},
	}
	runner, err := NewRunner(RunnerConfig{
		Root: t.TempDir(), RunID: "run-failed", Model: "fake-model",
		Agent: func(_ context.Context, _ AgentRequest, workspace, _ string) (AgentOutcome, error) {
			if err := os.WriteFile(filepath.Join(workspace, "partial.txt"), []byte("useful partial work\n"), 0o600); err != nil {
				return AgentOutcome{}, err
			}
			return AgentOutcome{Status: "failed"}, errors.New("Agent failed after writing a patch")
		},
		Verifier: func(context.Context, string, string, []Check) ([]CheckResult, error) {
			return []CheckResult{{Name: "tests", Passed: true}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := runner.RunTask(context.Background(), task)
	if result.Resolved || !strings.Contains(result.Patch, "partial.txt") || result.Agent.Status != "failed" {
		t.Fatalf("result = %#v", result)
	}
}

func newEvaluationRepository(t *testing.T) (string, string) {
	t.Helper()
	repository := t.TempDir()
	for _, arguments := range [][]string{
		{"init"},
		{"config", "user.email", "evaluation@example.test"},
		{"config", "user.name", "GSEA Evaluation"},
	} {
		if _, err := runGit(context.Background(), repository, arguments...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repository, "tracked.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(context.Background(), repository, "add", "tracked.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(context.Background(), repository, "commit", "-m", "base"); err != nil {
		t.Fatal(err)
	}
	base, err := runGit(context.Background(), repository, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return repository, strings.TrimSpace(string(base))
}

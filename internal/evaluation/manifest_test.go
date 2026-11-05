package evaluation

import (
	"bytes"
	"strings"
	"testing"
)

func TestLoadTasksValidatesJSONLAndDuplicateIDs(t *testing.T) {
	manifest := `{"schema":"gsea.eval.task.v1","id":"task-1","suite":"custom","kind":"patch","repository":"/tmp/repo","base_commit":"HEAD","problem_statement":"fix it","checks":[{"name":"tests","command":"go test ./..."}]}`
	tasks, err := LoadTasks(strings.NewReader(manifest + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].ID != "task-1" {
		t.Fatalf("tasks = %#v", tasks)
	}
	if _, err := LoadTasks(strings.NewReader(manifest + "\n" + manifest + "\n")); err == nil || !strings.Contains(err.Error(), "duplicate task id") {
		t.Fatalf("duplicate error = %v", err)
	}
	unknown := strings.Replace(manifest, `"id":"task-1"`, `"id":"task-2","unknown":true`, 1)
	if _, err := LoadTasks(strings.NewReader(unknown)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field error = %v", err)
	}
	if _, err := LoadTasks(strings.NewReader(manifest + ` {"schema":"ignored"}`)); err == nil || !strings.Contains(err.Error(), "multiple JSON values") {
		t.Fatalf("trailing JSON error = %v", err)
	}
}

func TestTaskRequiresANonOptionalCheck(t *testing.T) {
	task := Task{
		Schema: TaskSchema, ID: "task-1", Suite: "custom", Kind: "patch",
		Repository: "/tmp/repo", BaseCommit: "HEAD", ProblemStatement: "fix it",
		Checks: []Check{{Name: "diagnostic", Command: "true", Optional: true}},
	}
	if err := task.Validate(); err == nil || !strings.Contains(err.Error(), "non-optional") {
		t.Fatalf("validation error = %v", err)
	}
}

func TestTaskValidatesWorkspaceModes(t *testing.T) {
	base := Task{
		Schema: TaskSchema, ID: "task-1", Suite: "custom", Kind: "patch",
		Repository: "/tmp/repo", ProblemStatement: "fix it", Checks: []Check{{Name: "tests", Command: "true"}},
	}
	directory := base
	directory.WorkspaceMode = WorkspaceDirectory
	if err := directory.Validate(); err != nil {
		t.Fatalf("directory_copy validation: %v", err)
	}
	git := base
	git.WorkspaceMode = WorkspaceGitWorktree
	if err := git.Validate(); err == nil || !strings.Contains(err.Error(), "base_commit") {
		t.Fatalf("git validation error = %v", err)
	}
	unsupported := base
	unsupported.WorkspaceMode = "container"
	if err := unsupported.Validate(); err == nil || !strings.Contains(err.Error(), "workspace_mode") {
		t.Fatalf("unsupported validation error = %v", err)
	}
}

func TestLoadResultsRejectsMultipleValuesPerLine(t *testing.T) {
	input := `{"schema":"gsea.eval.result.v1","task_id":"task-1"} {"schema":"gsea.eval.result.v1","task_id":"ignored"}`
	if _, err := LoadResults(strings.NewReader(input)); err == nil || !strings.Contains(err.Error(), "multiple JSON values") {
		t.Fatalf("trailing result error = %v", err)
	}
}

func TestWriteSWEPredictionsUsesInstanceAndModelOverrides(t *testing.T) {
	results := []Result{{
		Schema: ResultSchema, TaskID: "task-1", InstanceID: "repo__1", Model: "original", Patch: "diff --git ...",
	}}
	var output bytes.Buffer
	if err := WriteSWEPredictions(&output, results, "gsea-model"); err != nil {
		t.Fatal(err)
	}
	want := `{"instance_id":"repo__1","model_name_or_path":"gsea-model","model_patch":"diff --git ..."}` + "\n"
	if output.String() != want {
		t.Fatalf("prediction = %q, want %q", output.String(), want)
	}
}

func TestChecksResolvedIgnoresOnlyOptionalFailures(t *testing.T) {
	if !ChecksResolved([]CheckResult{{Passed: true}, {Optional: true, Passed: false}}) {
		t.Fatal("optional failure should not make the task unresolved")
	}
	if ChecksResolved([]CheckResult{{Passed: false}}) || ChecksResolved(nil) {
		t.Fatal("required failure or missing checks must not resolve")
	}
}

func TestDockerCheckArgumentsKeepVerifierOfflineAndUnprivileged(t *testing.T) {
	arguments, err := dockerCheckArguments(t.TempDir(), Check{Image: "example/eval@sha256:abc", Command: "go test ./..."})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(arguments, " ")
	for _, expected := range []string{"--pull never", "--network none", "--cap-drop ALL", "no-new-privileges", "--pids-limit 512", "--entrypoint /bin/sh"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("docker arguments %q do not contain %q", joined, expected)
		}
	}
	if _, err := dockerCheckArguments(t.TempDir(), Check{Image: "--privileged", Command: "true"}); err == nil {
		t.Fatal("expected option-like Docker image to be rejected")
	}
}

func TestSummarizeAggregatesAttemptsUsageAndObservedPassAtK(t *testing.T) {
	results := []Result{
		{RunID: "run-1", TaskID: "task-a", Suite: "swe", Language: "python", Resolved: false, Agent: AgentOutcome{Status: "failed", Usage: Usage{Input: 10, Total: 12}}, Checks: []CheckResult{{Passed: false}}, DurationMS: 100},
		{RunID: "run-2", TaskID: "task-a", Suite: "swe", Language: "python", Resolved: true, Agent: AgentOutcome{Status: "completed", Usage: Usage{Input: 20, Output: 5, Total: 25, CostMicros: 7}}, Checks: []CheckResult{{Passed: true}}, DurationMS: 200},
		{RunID: "run-2", TaskID: "task-b", Suite: "terminal", Language: "shell", Resolved: false, Agent: AgentOutcome{Status: "timed_out"}, Checks: []CheckResult{{Passed: false}}, DurationMS: 300},
	}
	summary := Summarize(results)
	if summary.Tasks != 3 || summary.UniqueTasks != 2 || summary.Resolved != 1 || summary.Failed != 2 {
		t.Fatalf("summary counts = %#v", summary)
	}
	if summary.TasksWithSuccess != 1 || summary.ObservedPassAtK != 50 || summary.MinimumAttempts != 1 || summary.MaximumAttempts != 2 {
		t.Fatalf("pass@k summary = %#v", summary)
	}
	if summary.AgentFailures != 1 || summary.AgentTimeouts != 1 || summary.CheckFailures != 2 || summary.Usage.Input != 30 || summary.Usage.CostMicros != 7 || summary.DurationMS != 600 {
		t.Fatalf("aggregate summary = %#v", summary)
	}
}

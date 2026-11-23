package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"

	"github.com/vkviyu/go-self-evolving-agent/internal/evaluation"
	"github.com/vkviyu/go-self-evolving-agent/internal/modelgateway"
)

func TestEvalValidateAndExportSWECommands(t *testing.T) {
	manifestPath := filepath.Join(t.TempDir(), "tasks.jsonl")
	manifest := `{"schema":"gsea.eval.task.v1","id":"repo__1","instance_id":"repo__1","suite":"swebench","kind":"patch","language":"python","repository":"/tmp/repo","base_commit":"HEAD","problem_statement":"fix it","checks":[{"name":"tests","command":"pytest"}]}` + "\n"
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if exitCode := runWithDependencies(context.Background(), []string{"eval", "validate", "--tasks", manifestPath}, bytes.NewReader(nil), &stdout, &stderr, dependencies{}); exitCode != 0 {
		t.Fatalf("validate exit=%d stderr=%s", exitCode, stderr.String())
	}
	var validation struct {
		Valid bool `json:"valid"`
		Tasks int  `json:"tasks"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &validation); err != nil || !validation.Valid || validation.Tasks != 1 {
		t.Fatalf("validation=%+v err=%v output=%s", validation, err, stdout.String())
	}

	resultsPath := filepath.Join(t.TempDir(), "results.jsonl")
	var results bytes.Buffer
	if err := evaluation.WriteResult(&results, evaluation.Result{
		Schema: evaluation.ResultSchema, TaskID: "repo__1", InstanceID: "repo__1", Model: "gsea", Patch: "diff --git ...",
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resultsPath, results.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if exitCode := runWithDependencies(context.Background(), []string{"eval", "export-swebench", "--results", resultsPath, "--model", "gsea-eval"}, bytes.NewReader(nil), &stdout, &stderr, dependencies{}); exitCode != 0 {
		t.Fatalf("export exit=%d stderr=%s", exitCode, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, `"instance_id":"repo__1"`) || !strings.Contains(got, `"model_name_or_path":"gsea-eval"`) {
		t.Fatalf("prediction output = %q", got)
	}
}

func TestEvalDoctorReportsCapabilitySchema(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := runEval(context.Background(), []string{"doctor"}, &stdout, &stderr, dependencies{}); err != nil {
		t.Fatal(err)
	}
	var report evaluation.Capabilities
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode capabilities: %v\n%s", err, stdout.String())
	}
	if report.Schema != evaluation.CapabilitiesSchema || report.OS == "" || report.Architecture == "" || len(report.Commands) == 0 {
		t.Fatalf("capabilities = %#v", report)
	}
}

func TestEvalRunOwnsIsolationArguments(t *testing.T) {
	for _, argument := range []string{"--workspace", "--data-dir", "--session", "--evaluation-mode"} {
		var stdout, stderr bytes.Buffer
		err := runEval(context.Background(), []string{"run", "--tasks", "missing.jsonl", "--", argument, "value"}, &stdout, &stderr, dependencies{})
		if err == nil || !strings.Contains(err.Error(), "owned by the evaluator") {
			t.Fatalf("argument %s error = %v", argument, err)
		}
	}
}

func TestExecuteEvaluationTasksUsesBoundedParallelism(t *testing.T) {
	tasks := []evaluation.Task{{ID: "one"}, {ID: "two"}, {ID: "three"}, {ID: "four"}}
	var active, maximum atomic.Int32
	run := func(ctx context.Context, task evaluation.Task) evaluation.Result {
		current := active.Add(1)
		for observed := maximum.Load(); current > observed && !maximum.CompareAndSwap(observed, current); observed = maximum.Load() {
		}
		select {
		case <-ctx.Done():
		case <-time.After(20 * time.Millisecond):
		}
		active.Add(-1)
		return evaluation.Result{TaskID: task.ID}
	}
	consumed := make(map[string]struct{})
	results, err := executeEvaluationTasks(context.Background(), tasks, 2, run, func(result evaluation.Result) error {
		consumed[result.TaskID] = struct{}{}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != len(tasks) || len(consumed) != len(tasks) || maximum.Load() != 2 {
		t.Fatalf("results=%d consumed=%d maximum=%d", len(results), len(consumed), maximum.Load())
	}
}

func TestExecuteEvaluationTasksCancelsAfterConsumerFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	tasks := []evaluation.Task{{ID: "one"}, {ID: "two"}, {ID: "three"}}
	var consumed atomic.Int32
	_, err := executeEvaluationTasks(ctx, tasks, 1, func(ctx context.Context, task evaluation.Task) evaluation.Result {
		return evaluation.Result{TaskID: task.ID}
	}, func(evaluation.Result) error {
		consumed.Add(1)
		return errors.New("disk full")
	})
	if err == nil || !strings.Contains(err.Error(), "disk full") || consumed.Load() != 1 {
		t.Fatalf("consumed=%d err=%v", consumed.Load(), err)
	}
}

func TestParseHeadlessUsageAggregatesModelRequests(t *testing.T) {
	usage := parseHeadlessUsage(strings.Join([]string{
		"usage: input=10 cached=4 cache_hit=40.0% output=3 reasoning=2 total=13",
		"usage: input=20 output=5 reasoning=0 total=25",
	}, "\n"))
	if usage.Input != 30 || usage.Cached != 4 || usage.Output != 8 || usage.Reasoning != 2 || usage.Total != 38 {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestAssertOrFillScoreLabelRejectsContradictoryMetadata(t *testing.T) {
	recorded := "openai/gpt-5.6-sol"
	if err := assertOrFillScoreLabel(&recorded, "openai/gpt-5.6-sol", "model"); err != nil {
		t.Fatal(err)
	}
	if err := assertOrFillScoreLabel(&recorded, "openai/gpt-5.6-terra", "model"); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("error = %v", err)
	}
	empty := ""
	if err := assertOrFillScoreLabel(&empty, "0.22.0", "harness version"); err != nil || empty != "0.22.0" {
		t.Fatalf("empty=%q error=%v", empty, err)
	}
}

func TestEvaluationSessionIDDoesNotCollapseSanitizedTaskIDs(t *testing.T) {
	first := evaluationSessionID("run", "owner/repo")
	second := evaluationSessionID("run", "owner_repo")
	if first == second || len(first) > 128 || len(second) > 128 {
		t.Fatalf("session IDs = %q and %q", first, second)
	}
}

func TestEvalRunExecutesAgentAndCapturesPatch(t *testing.T) {
	repository, base := newCLIEvaluationRepository(t)
	manifestPath := filepath.Join(t.TempDir(), "tasks.jsonl")
	task := evaluation.Task{
		Schema: evaluation.TaskSchema, ID: "cli-eval-1", Suite: "custom", Kind: "patch", Language: "go",
		Repository: repository, BaseCommit: base,
		ProblemStatement: "Create answer.go.\nPreserve this second line in the Agent prompt.",
		Checks:           []evaluation.Check{{Name: "environment-probe", Command: "true"}},
	}
	encoded, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cliATEEnvironment(t)
	fakeModel := &workspaceWriteChatModel{}
	var stdout, stderr bytes.Buffer
	err = runEval(context.Background(), []string{
		"run", "--tasks", manifestPath, "--root", root, "--run-id", "cli-run", "--model-label", "fake-model",
		"--", "--model", "fake-model",
	}, &stdout, &stderr, dependencies{newModel: func(context.Context, modelgateway.Config) (model.ToolCallingChatModel, error) {
		return fakeModel, nil
	}, evalVerifier: func(_ context.Context, workspace, _ string, checks []evaluation.Check) ([]evaluation.CheckResult, error) {
		if len(checks) != 1 || checks[0].Name != "environment-probe" {
			t.Fatalf("checks = %#v", checks)
		}
		if _, err := os.Stat(filepath.Join(workspace, "answer.go")); err != nil {
			return nil, err
		}
		return []evaluation.CheckResult{{Name: checks[0].Name, Passed: true}}, nil
	}})
	if err != nil {
		t.Fatalf("eval run: %v\nstderr: %s", err, stderr.String())
	}
	if fakeModel.lastUserContent != task.ProblemStatement {
		t.Fatalf("Agent prompt = %q, want %q", fakeModel.lastUserContent, task.ProblemStatement)
	}
	var summary struct {
		ResultsPath string `json:"results_path"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &summary); err != nil {
		t.Fatalf("decode summary: %v\n%s", err, stdout.String())
	}
	resultsFile, err := os.Open(summary.ResultsPath)
	if err != nil {
		t.Fatal(err)
	}
	results, loadErr := evaluation.LoadResults(resultsFile)
	closeErr := resultsFile.Close()
	if loadErr != nil || closeErr != nil {
		t.Fatal(errors.Join(loadErr, closeErr))
	}
	if len(results) != 1 || !results[0].Resolved || !strings.Contains(results[0].Patch, "answer.go") || !strings.Contains(results[0].Patch, "+package answer") {
		t.Fatalf("results = %#v", results)
	}
}

func newCLIEvaluationRepository(t *testing.T) (string, string) {
	t.Helper()
	repository := t.TempDir()
	for _, arguments := range [][]string{
		{"init"},
		{"config", "user.email", "evaluation@example.test"},
		{"config", "user.name", "GSEA Evaluation"},
	} {
		command := exec.Command("git", arguments...)
		command.Dir = repository
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("baseline\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{{"add", "README.md"}, {"commit", "-m", "baseline"}} {
		command := exec.Command("git", arguments...)
		command.Dir = repository
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
	}
	command := exec.Command("git", "rev-parse", "HEAD")
	command.Dir = repository
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	return repository, strings.TrimSpace(string(output))
}

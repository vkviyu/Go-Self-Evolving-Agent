package evaluation

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestScoreTerminalBenchMatchesOfficialFormula(t *testing.T) {
	trials := make([]BinaryTrial, 0, 10)
	for index := range 5 {
		trials = append(trials, rewardedTrial("task-a", index < 4))
		trials = append(trials, rewardedTrial("task-b", index < 2))
	}
	score, err := ScoreTerminalBench(trials)
	if err != nil {
		t.Fatal(err)
	}
	if score.PrimaryMetric.ValuePercent != 60 || score.PrimaryMetric.StandardErrorPercent == nil {
		t.Fatalf("primary metric = %#v", score.PrimaryMetric)
	}
	if *score.PrimaryMetric.StandardErrorPercent != 15.81 {
		t.Fatalf("standard error = %.2f", *score.PrimaryMetric.StandardErrorPercent)
	}
	expected := map[string]float64{"pass_at_2": 0.85, "pass_at_3": 0.95, "pass_at_4": 1, "pass_at_5": 1}
	for name, value := range expected {
		if difference := math.Abs(score.PassAtK[name] - value); difference > 1e-12 {
			t.Fatalf("%s = %.15f, want %.15f", name, score.PassAtK[name], value)
		}
	}
	if score.Compliance.Comparable || len(score.Compliance.Violations) == 0 {
		t.Fatalf("partial score unexpectedly comparable: %#v", score.Compliance)
	}
}

func TestScoreTerminalBenchFullCoverageIsComparable(t *testing.T) {
	trials := make([]BinaryTrial, 0, 89*5)
	for task := range 89 {
		for range 5 {
			trials = append(trials, rewardedTrial(stringID(task), true))
		}
	}
	score, err := ScoreTerminalBench(trials)
	if err != nil {
		t.Fatal(err)
	}
	if !score.Compliance.Comparable || score.Coverage.Trials != 445 || score.PrimaryMetric.ValuePercent != 100 {
		t.Fatalf("score = %#v", score)
	}
}

func TestLoadHarborJobCountsErrorsAsFailures(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "trial-a")
	second := filepath.Join(root, "trial-b")
	if err := os.MkdirAll(first, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(second, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(first, "result.json"), `{
  "id":"one","task_name":"task-a","source":"terminal-bench/terminal-bench-2-1",
  "agent_info":{"name":"gsea","version":"dev","model_info":{"name":"gpt-5.6-sol","provider":"openai"}},
  "verifier_result":{"rewards":{"reward":1}},"exception_info":null
}`)
	writeTestFile(t, filepath.Join(second, "result.json"), `{
  "id":"two","task_name":"task-b","source":"terminal-bench/terminal-bench-2-1",
  "agent_info":{"name":"gsea","version":"dev","model_info":{"name":"gpt-5.6-sol","provider":"openai"}},
  "verifier_result":null,"exception_info":{"exception_type":"AgentTimeoutError"}
}`)
	trials, metadata, err := LoadHarborJob(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(trials) != 2 || metadata.Agent != "gsea" || metadata.Model != "openai/gpt-5.6-sol" {
		t.Fatalf("trials=%#v metadata=%#v", trials, metadata)
	}
	score, err := ScoreTerminalBench(trials)
	if err != nil {
		t.Fatal(err)
	}
	if score.PrimaryMetric.ValuePercent != 50 || score.Coverage.ErroredTrials != 1 {
		t.Fatalf("score = %#v", score)
	}
}

func TestHarborProtocolEvidenceUsesPinnedConfigAndLock(t *testing.T) {
	root := t.TempDir()
	trialDirectory := filepath.Join(root, "trial-a")
	if err := os.MkdirAll(trialDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, "config.json"), `{
  "datasets":[{"name":"terminal-bench/terminal-bench-2-1","ref":"sha256:7d7bdc1cbedad549fc1140404bd4dc45e5fd0ea7c4186773687d177ad3a0699a"}],
  "agents":[{"model_name":"openai/gpt-5.6-sol","kwargs":{"reasoning_effort":"high"}}]
}`)
	writeTestFile(t, filepath.Join(root, "lock.json"), `{
  "harbor":{"version":"0.22.0"},
  "trials":[{
    "timeout_multiplier":1,
    "task":{"name":"terminal-bench/task-a","digest":"sha256:task-a","source":"terminal-bench/terminal-bench-2-1"},
    "agent":{"model_name":"openai/gpt-5.6-sol","kwargs":{"reasoning_effort":"high"}},
    "environment":{},"verifier":{"disable":false}
  }]
}`)
	writeTestFile(t, filepath.Join(trialDirectory, "result.json"), `{
  "id":"one","task_name":"terminal-bench/task-a","source":"terminal-bench/terminal-bench-2-1",
  "task_id":{"ref":"sha256:task-a"},
  "agent_info":{"name":"gsea","version":"dev","model_info":{"name":"gpt-5.6-sol","provider":"openai"}},
  "verifier_result":null,"exception_info":{"exception_type":"AgentTimeoutError"}
}`)
	trials, metadata, err := LoadHarborJob(root)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.DatasetRef != TerminalBench21DatasetRef || metadata.HarnessVersion != "0.22.0" || metadata.ReasoningEffort != "high" || len(metadata.ProtocolViolations) != 0 {
		t.Fatalf("metadata = %#v", metadata)
	}
	score, err := ScoreTerminalBench(trials)
	if err != nil {
		t.Fatal(err)
	}
	ApplyHarborProtocolMetadata(&score, metadata)
	if score.Compliance.Comparable || score.HarnessVersion != "0.22.0" || len(score.Compliance.Violations) != 2 {
		t.Fatalf("score = %#v", score)
	}
}

func TestHarborProtocolEvidenceRejectsTimeoutOverrides(t *testing.T) {
	root := t.TempDir()
	trialDirectory := filepath.Join(root, "trial-a")
	if err := os.MkdirAll(trialDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, "config.json"), `{
  "datasets":[{"name":"terminal-bench/terminal-bench-2-1","ref":"sha256:7d7bdc1cbedad549fc1140404bd4dc45e5fd0ea7c4186773687d177ad3a0699a"}]
}`)
	writeTestFile(t, filepath.Join(root, "lock.json"), `{
  "harbor":{"version":"0.22.0"},
  "trials":[{
    "timeout_multiplier":2,
    "task":{"name":"terminal-bench/task-a","digest":"sha256:task-a","source":"terminal-bench/terminal-bench-2-1"},
    "agent":{},"environment":{},"verifier":{"disable":false}
  }]
}`)
	writeTestFile(t, filepath.Join(trialDirectory, "result.json"), `{
  "id":"one","task_name":"terminal-bench/task-a","source":"terminal-bench/terminal-bench-2-1",
  "task_id":{"ref":"sha256:task-a"},
  "verifier_result":null,"exception_info":{"exception_type":"AgentTimeoutError"}
}`)
	_, metadata, err := LoadHarborJob(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(metadata.ProtocolViolations) != 1 || !strings.Contains(metadata.ProtocolViolations[0], "timeout_multiplier") {
		t.Fatalf("metadata = %#v", metadata)
	}
}

func TestHarborProtocolEvidenceRejectsDigestMismatch(t *testing.T) {
	root := t.TempDir()
	trialDirectory := filepath.Join(root, "trial-a")
	if err := os.MkdirAll(trialDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, "config.json"), `{
  "datasets":[{"name":"terminal-bench/terminal-bench-2-1","ref":"sha256:7d7bdc1cbedad549fc1140404bd4dc45e5fd0ea7c4186773687d177ad3a0699a"}]
}`)
	writeTestFile(t, filepath.Join(root, "lock.json"), `{
  "harbor":{"version":"0.22.0"},
  "trials":[{
    "timeout_multiplier":1,
    "task":{"name":"terminal-bench/task-a","digest":"sha256:canonical","source":"terminal-bench/terminal-bench-2-1"},
    "agent":{},"environment":{},"verifier":{"disable":false}
  }]
}`)
	writeTestFile(t, filepath.Join(trialDirectory, "result.json"), `{
  "id":"one","task_name":"terminal-bench/task-a","source":"terminal-bench/terminal-bench-2-1",
  "task_id":{"ref":"sha256:tampered"},
  "verifier_result":null,"exception_info":{"exception_type":"AgentTimeoutError"}
}`)
	_, metadata, err := LoadHarborJob(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(metadata.ProtocolViolations) != 1 || !strings.Contains(metadata.ProtocolViolations[0], "digest") {
		t.Fatalf("metadata = %#v", metadata)
	}
}

func TestLoadHarborJobsCombinesResumableBatches(t *testing.T) {
	first := newHarborJobFixture(t, "trial-one", "terminal-bench/task-a", "sha256:task-a")
	second := newHarborJobFixture(t, "trial-two", "terminal-bench/task-b", "sha256:task-b")
	trials, metadata, err := LoadHarborJobs([]string{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if len(trials) != 2 || metadata.Model != "openai/gpt-5.6-sol" || metadata.ReasoningEffort != "high" || len(metadata.ProtocolViolations) != 0 {
		t.Fatalf("trials=%#v metadata=%#v", trials, metadata)
	}
}

func TestLoadHarborJobsRejectsDuplicateTrialIDs(t *testing.T) {
	first := newHarborJobFixture(t, "duplicate", "terminal-bench/task-a", "sha256:task-a")
	second := newHarborJobFixture(t, "duplicate", "terminal-bench/task-b", "sha256:task-b")
	trials, metadata, err := LoadHarborJobs([]string{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if len(trials) != 1 || len(metadata.ProtocolViolations) != 1 || !strings.Contains(metadata.ProtocolViolations[0], "duplicate") {
		t.Fatalf("trials=%#v metadata=%#v", trials, metadata)
	}
}

func TestTerminalBenchAuditDisqualifiesSuccessfulTrial(t *testing.T) {
	auditPath := filepath.Join(t.TempDir(), "audit.json")
	writeTestFile(t, auditPath, `{"disqualified_trials":[{"trial_id":"trial-1","reason":"reward_hacking"}]}`)
	one := 1.0
	trials := []BinaryTrial{
		{TrialID: "trial-1", TaskID: "task-a", Reward: &one},
		{TrialID: "trial-2", TaskID: "task-b", Reward: &one},
	}
	count, err := ApplyTerminalBenchAudit(trials, auditPath)
	if err != nil {
		t.Fatal(err)
	}
	score, err := ScoreTerminalBench(trials)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || score.PrimaryMetric.Successful != 1 || score.PrimaryMetric.ValuePercent != 50 || score.Coverage.DisqualifiedTrials != 1 {
		t.Fatalf("count=%d score=%#v", count, score)
	}
}

func TestTerminalBenchAuditRejectsAnotherRunsTrialIDs(t *testing.T) {
	auditPath := filepath.Join(t.TempDir(), "audit.json")
	writeTestFile(t, auditPath, `["unknown-trial"]`)
	one := 1.0
	trials := []BinaryTrial{{TrialID: "trial-1", TaskID: "task-a", Reward: &one}}
	if _, err := ApplyTerminalBenchAudit(trials, auditPath); err == nil || !strings.Contains(err.Error(), "unknown trial IDs") {
		t.Fatalf("error = %v", err)
	}
}

func TestScoreSWEBenchVerifiedRequiresFullSubmission(t *testing.T) {
	score := ScoreSWEBenchVerified(SWEBenchRunReport{
		TotalInstances: 500, SubmittedInstances: 500, CompletedInstances: 490,
		ResolvedInstances: 413, ErrorInstances: 10, SchemaVersion: 2,
	})
	if !score.Compliance.Comparable || score.PrimaryMetric.ValuePercent != 82.6 {
		t.Fatalf("score = %#v", score)
	}
	partial := ScoreSWEBenchVerified(SWEBenchRunReport{
		TotalInstances: 1, SubmittedInstances: 1, CompletedInstances: 1, ResolvedInstances: 1,
	})
	if partial.Compliance.Comparable || partial.PrimaryMetric.ValuePercent != 100 {
		t.Fatalf("partial score = %#v", partial)
	}
}

func rewardedTrial(task string, success bool) BinaryTrial {
	reward := 0.0
	if success {
		reward = 1
	}
	return BinaryTrial{TaskID: task, Reward: &reward}
}

func stringID(value int) string {
	return "task-" + strconv.Itoa(value)
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newHarborJobFixture(t *testing.T, trialID, taskName, digest string) string {
	t.Helper()
	root := t.TempDir()
	trialDirectory := filepath.Join(root, "trial")
	if err := os.MkdirAll(trialDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, "config.json"), `{
  "datasets":[{"name":"terminal-bench/terminal-bench-2-1","ref":"sha256:7d7bdc1cbedad549fc1140404bd4dc45e5fd0ea7c4186773687d177ad3a0699a"}],
  "agents":[{"model_name":"openai/gpt-5.6-sol","kwargs":{"reasoning_effort":"high"}}]
}`)
	writeTestFile(t, filepath.Join(root, "lock.json"), fmt.Sprintf(`{
  "harbor":{"version":"0.22.0"},
  "trials":[{
    "timeout_multiplier":1,
    "task":{"name":%q,"digest":%q,"source":"terminal-bench/terminal-bench-2-1"},
    "agent":{"model_name":"openai/gpt-5.6-sol","kwargs":{"reasoning_effort":"high"}},
    "environment":{},"verifier":{"disable":false}
  }]
}`, taskName, digest))
	writeTestFile(t, filepath.Join(trialDirectory, "result.json"), fmt.Sprintf(`{
  "id":%q,"task_name":%q,"source":"terminal-bench/terminal-bench-2-1",
  "task_id":{"ref":%q},
  "agent_info":{"name":"gsea","version":"dev","model_info":{"name":"gpt-5.6-sol","provider":"openai"}},
  "verifier_result":{"rewards":{"reward":0}},"exception_info":null
}`, trialID, taskName, digest))
	return root
}

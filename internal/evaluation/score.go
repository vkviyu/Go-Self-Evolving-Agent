package evaluation

import (
	"errors"
	"fmt"
	"math"
	"sort"
)

const ScorecardSchema = "gsea.eval.scorecard.v1"

const (
	BenchmarkTerminalBench21  = "terminal-bench-2.1"
	BenchmarkSWEBenchVerified = "swe-bench-verified"
	// TerminalBench21DatasetRef pins the exact 89-task release used by the
	// Terminal-Bench 2.1 leaderboard rather than following a mutable alias.
	TerminalBench21DatasetRef = "terminal-bench/terminal-bench-2-1@sha256:7d7bdc1cbedad549fc1140404bd4dc45e5fd0ea7c4186773687d177ad3a0699a"
)

// BenchmarkProtocol records the immutable parts of a leaderboard comparison.
// A score is comparable only when the coverage requirements are satisfied and
// the official verifier produced every reward used by the metric.
type BenchmarkProtocol struct {
	Benchmark              string `json:"benchmark"`
	Dataset                string `json:"dataset"`
	DatasetRef             string `json:"dataset_ref,omitempty"`
	Harness                string `json:"harness"`
	PrimaryMetric          string `json:"primary_metric"`
	ExpectedTasks          int    `json:"expected_tasks"`
	MinimumAttemptsPerTask int    `json:"minimum_attempts_per_task"`
}

type ScoreMetric struct {
	Name                 string   `json:"name"`
	ValuePercent         float64  `json:"value_percent"`
	StandardErrorPercent *float64 `json:"standard_error_percent,omitempty"`
	Successful           int      `json:"successful"`
	Total                int      `json:"total"`
}

type ScoreCoverage struct {
	ExpectedTasks          int `json:"expected_tasks"`
	ObservedTasks          int `json:"observed_tasks"`
	MinimumAttemptsPerTask int `json:"minimum_attempts_per_task"`
	MaximumAttemptsPerTask int `json:"maximum_attempts_per_task"`
	Trials                 int `json:"trials"`
	SuccessfulTrials       int `json:"successful_trials"`
	FailedTrials           int `json:"failed_trials"`
	ErroredTrials          int `json:"errored_trials"`
	DisqualifiedTrials     int `json:"disqualified_trials"`
}

type ProtocolCompliance struct {
	// Comparable means the local metric has the official denominator, trial
	// count, binary reward rule, and aggregation formula. It is deliberately
	// separate from OfficialAuditPassed: a local result is not a leaderboard
	// certification until the benchmark owner validates artifacts and traces.
	Comparable          bool     `json:"comparable"`
	OfficialAuditPassed bool     `json:"official_audit_passed"`
	Violations          []string `json:"violations,omitempty"`
	Warnings            []string `json:"warnings,omitempty"`
}

// Scorecard is a benchmark-neutral envelope around an official scorer's
// binary outcomes. PrimaryMetric always describes the observed data. Callers
// must check Compliance.Comparable before presenting it as a leaderboard-level
// result.
type Scorecard struct {
	Schema          string             `json:"schema"`
	Protocol        BenchmarkProtocol  `json:"protocol"`
	HarnessVersion  string             `json:"harness_version,omitempty"`
	Agent           string             `json:"agent,omitempty"`
	AgentVersion    string             `json:"agent_version,omitempty"`
	Model           string             `json:"model,omitempty"`
	ReasoningEffort string             `json:"reasoning_effort,omitempty"`
	PrimaryMetric   ScoreMetric        `json:"primary_metric"`
	PassAtK         map[string]float64 `json:"pass_at_k,omitempty"`
	Coverage        ScoreCoverage      `json:"coverage"`
	Compliance      ProtocolCompliance `json:"protocol_compliance"`
	Metadata        map[string]any     `json:"metadata,omitempty"`
}

// BinaryTrial is the minimal, scorer-owned view of one official verifier
// outcome. Reward is nil for an errored trial and still counts as a failure.
type BinaryTrial struct {
	TrialID      string
	TaskID       string
	Reward       *float64
	Errored      bool
	Disqualified bool
}

// ProtocolForBenchmark returns the dataset, harness and scoring protocol associated with a
// supported benchmark.
func ProtocolForBenchmark(name string) (BenchmarkProtocol, error) {
	switch name {
	case BenchmarkTerminalBench21:
		return BenchmarkProtocol{
			Benchmark:              BenchmarkTerminalBench21,
			Dataset:                "terminal-bench/terminal-bench-2-1",
			DatasetRef:             TerminalBench21DatasetRef,
			Harness:                "harbor",
			PrimaryMetric:          "accuracy",
			ExpectedTasks:          89,
			MinimumAttemptsPerTask: 5,
		}, nil
	case BenchmarkSWEBenchVerified:
		return BenchmarkProtocol{
			Benchmark:              BenchmarkSWEBenchVerified,
			Dataset:                "SWE-bench/SWE-bench_Verified",
			Harness:                "swebench",
			PrimaryMetric:          "pass@1",
			ExpectedTasks:          500,
			MinimumAttemptsPerTask: 1,
		}, nil
	default:
		return BenchmarkProtocol{}, fmt.Errorf("unsupported benchmark protocol %q", name)
	}
}

// ScoreTerminalBench implements the Terminal-Bench 2.1 leaderboard formulas:
// accuracy over every trial, per-task standard error, and the unbiased pass@k
// estimator averaged over tasks. It intentionally treats missing rewards and
// errored trials as failures.
func ScoreTerminalBench(trials []BinaryTrial) (Scorecard, error) {
	protocol, _ := ProtocolForBenchmark(BenchmarkTerminalBench21)
	if len(trials) == 0 {
		return Scorecard{}, errors.New("Terminal-Bench score requires at least one trial")
	}
	byTask := make(map[string][]BinaryTrial)
	for index, trial := range trials {
		if trial.TaskID == "" {
			return Scorecard{}, fmt.Errorf("trial %d has an empty task ID", index)
		}
		byTask[trial.TaskID] = append(byTask[trial.TaskID], trial)
	}

	successful, errored, disqualified := 0, 0, 0
	for _, trial := range trials {
		if trialSucceeded(trial) {
			successful++
		}
		if trial.Errored || trial.Reward == nil {
			errored++
		}
		if trial.Disqualified {
			disqualified++
		}
	}
	accuracy := roundTo(100*float64(successful)/float64(len(trials)), 2)
	standardError := roundTo(terminalBenchStandardError(byTask), 2)
	minimum, maximum := attemptRange(byTask)
	violations := terminalBenchCoverageViolations(protocol, byTask, minimum)

	return Scorecard{
		Schema:   ScorecardSchema,
		Protocol: protocol,
		PrimaryMetric: ScoreMetric{
			Name: protocol.PrimaryMetric, ValuePercent: accuracy,
			StandardErrorPercent: &standardError,
			Successful:           successful, Total: len(trials),
		},
		PassAtK: terminalBenchPassAtK(byTask, []int{2, 3, 4, 5}),
		Coverage: ScoreCoverage{
			ExpectedTasks: protocol.ExpectedTasks, ObservedTasks: len(byTask),
			MinimumAttemptsPerTask: minimum, MaximumAttemptsPerTask: maximum,
			Trials: len(trials), SuccessfulTrials: successful,
			FailedTrials: len(trials) - successful, ErroredTrials: errored,
			DisqualifiedTrials: disqualified,
		},
		Compliance: ProtocolCompliance{
			Comparable: len(violations) == 0,
			Violations: violations,
			Warnings: []string{
				"local score uses the official metric, but leaderboard certification additionally requires canonical task digests and trajectory/reward-hacking audit",
			},
		},
	}, nil
}

func trialSucceeded(trial BinaryTrial) bool {
	return !trial.Disqualified && trial.Reward != nil && *trial.Reward > 0
}

func terminalBenchStandardError(byTask map[string][]BinaryTrial) float64 {
	if len(byTask) == 0 {
		return 0
	}
	variance := 0.0
	for _, trials := range byTask {
		if len(trials) < 2 {
			continue
		}
		successful := 0
		for _, trial := range trials {
			if trialSucceeded(trial) {
				successful++
			}
		}
		p := float64(successful) / float64(len(trials))
		variance += p * (1 - p) / float64(len(trials)-1)
	}
	variance /= float64(len(byTask) * len(byTask))
	return 100 * math.Sqrt(variance)
}

func terminalBenchPassAtK(byTask map[string][]BinaryTrial, values []int) map[string]float64 {
	result := make(map[string]float64, len(values))
	for _, k := range values {
		var total float64
		eligible := 0
		for _, trials := range byTask {
			n := len(trials)
			if n < k {
				continue
			}
			c := 0
			for _, trial := range trials {
				if trialSucceeded(trial) {
					c++
				}
			}
			total += passAtKForTask(n, c, k)
			eligible++
		}
		if eligible > 0 {
			result[fmt.Sprintf("pass_at_%d", k)] = roundTo(total/float64(eligible), 4)
		}
	}
	return result
}

func roundTo(value float64, places int) float64 {
	scale := math.Pow10(places)
	return math.Round(value*scale) / scale
}

func passAtKForTask(n, c, k int) float64 {
	if n-c < k {
		return 1
	}
	miss := 1.0
	for index := range k {
		miss *= float64(n-c-index) / float64(n-index)
	}
	return 1 - miss
}

func attemptRange(byTask map[string][]BinaryTrial) (int, int) {
	minimum, maximum := 0, 0
	for _, trials := range byTask {
		if minimum == 0 || len(trials) < minimum {
			minimum = len(trials)
		}
		if len(trials) > maximum {
			maximum = len(trials)
		}
	}
	return minimum, maximum
}

func terminalBenchCoverageViolations(protocol BenchmarkProtocol, byTask map[string][]BinaryTrial, minimum int) []string {
	violations := make([]string, 0, 2)
	if len(byTask) != protocol.ExpectedTasks {
		violations = append(violations, fmt.Sprintf("expected %d tasks, observed %d", protocol.ExpectedTasks, len(byTask)))
	}
	if minimum < protocol.MinimumAttemptsPerTask {
		violations = append(violations, fmt.Sprintf("every task requires at least %d trials; observed minimum is %d", protocol.MinimumAttemptsPerTask, minimum))
	}
	sort.Strings(violations)
	return violations
}

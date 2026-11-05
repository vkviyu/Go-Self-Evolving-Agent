package evaluation

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const maximumOfficialResultBytes = 32 << 20

type HarborRunMetadata struct {
	Agent              string
	AgentVersion       string
	Model              string
	ReasoningEffort    string
	Source             string
	DatasetRef         string
	HarnessVersion     string
	ExceptionCounts    map[string]int
	ProtocolViolations []string
	ProtocolWarnings   []string
}

type harborJobConfig struct {
	Datasets []struct {
		Name string `json:"name"`
		Ref  string `json:"ref"`
	} `json:"datasets"`
	Agents []struct {
		ModelName string `json:"model_name"`
		Kwargs    struct {
			ReasoningEffort string `json:"reasoning_effort"`
		} `json:"kwargs"`
	} `json:"agents"`
}

type harborJobLock struct {
	Harbor struct {
		Version string `json:"version"`
	} `json:"harbor"`
	Trials []struct {
		Task struct {
			Name   string `json:"name"`
			Digest string `json:"digest"`
			Source string `json:"source"`
		} `json:"task"`
		TimeoutMultiplier            float64  `json:"timeout_multiplier"`
		AgentTimeoutMultiplier       *float64 `json:"agent_timeout_multiplier"`
		VerifierTimeoutMultiplier    *float64 `json:"verifier_timeout_multiplier"`
		SetupTimeoutMultiplier       *float64 `json:"agent_setup_timeout_multiplier"`
		EnvironmentTimeoutMultiplier *float64 `json:"environment_build_timeout_multiplier"`
		Agent                        struct {
			ModelName               string   `json:"model_name"`
			OverrideTimeoutSec      *float64 `json:"override_timeout_sec"`
			OverrideSetupTimeoutSec *float64 `json:"override_setup_timeout_sec"`
			MaxTimeoutSec           *float64 `json:"max_timeout_sec"`
			Kwargs                  struct {
				ReasoningEffort string `json:"reasoning_effort"`
			} `json:"kwargs"`
		} `json:"agent"`
		Environment struct {
			OverrideCPUs      *int `json:"override_cpus"`
			OverrideMemoryMB  *int `json:"override_memory_mb"`
			OverrideStorageMB *int `json:"override_storage_mb"`
			OverrideGPUs      *int `json:"override_gpus"`
		} `json:"environment"`
		Verifier struct {
			Disable            bool     `json:"disable"`
			OverrideTimeoutSec *float64 `json:"override_timeout_sec"`
			MaxTimeoutSec      *float64 `json:"max_timeout_sec"`
		} `json:"verifier"`
	} `json:"trials"`
}

type harborTrialResult struct {
	ID       string `json:"id"`
	TaskName string `json:"task_name"`
	Source   string `json:"source"`
	TaskID   struct {
		Ref string `json:"ref"`
	} `json:"task_id"`
	AgentInfo struct {
		Name      string `json:"name"`
		Version   string `json:"version"`
		ModelInfo *struct {
			Name     string `json:"name"`
			Provider string `json:"provider"`
		} `json:"model_info"`
	} `json:"agent_info"`
	VerifierResult *struct {
		Rewards map[string]json.Number `json:"rewards"`
	} `json:"verifier_result"`
	ExceptionInfo json.RawMessage `json:"exception_info"`
}

type harborJobResult struct {
	TrialResults []harborTrialResult `json:"trial_results"`
}

type terminalBenchAudit struct {
	DisqualifiedTrials *[]struct {
		TrialID string `json:"trial_id"`
	} `json:"disqualified_trials"`
}

// LoadHarborJob reads Harbor's durable per-trial result.json files. It also
// accepts an expanded job-level result containing trial_results, which makes
// the importer useful for downloaded Hub artifacts and local job directories.
func LoadHarborJob(root string) ([]BinaryTrial, HarborRunMetadata, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, HarborRunMetadata{}, err
	}
	paths := make([]string, 0)
	if !info.IsDir() {
		paths = append(paths, root)
	} else if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && entry.Name() == "result.json" {
			paths = append(paths, path)
		}
		return nil
	}); err != nil {
		return nil, HarborRunMetadata{}, err
	}
	if len(paths) == 0 {
		return nil, HarborRunMetadata{}, errors.New("Harbor job contains no result.json files")
	}

	seen := make(map[string]struct{})
	trials := make([]BinaryTrial, 0)
	metadata := HarborRunMetadata{}
	for _, path := range paths {
		payload, readErr := readBoundedJSON(path)
		if readErr != nil {
			return nil, HarborRunMetadata{}, readErr
		}
		var job harborJobResult
		if err := json.Unmarshal(payload, &job); err != nil {
			return nil, HarborRunMetadata{}, fmt.Errorf("decode Harbor result %s: %w", path, err)
		}
		candidates := job.TrialResults
		if len(candidates) == 0 {
			var trial harborTrialResult
			if err := json.Unmarshal(payload, &trial); err != nil {
				return nil, HarborRunMetadata{}, fmt.Errorf("decode Harbor trial %s: %w", path, err)
			}
			if trial.TaskName != "" {
				candidates = append(candidates, trial)
			}
		}
		for _, trial := range candidates {
			key := trial.ID
			if key == "" {
				key = path + "\x00" + trial.TaskName
			}
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			binary, convertErr := binaryTrialFromHarbor(trial)
			if convertErr != nil {
				return nil, HarborRunMetadata{}, fmt.Errorf("decode Harbor trial %s: %w", path, convertErr)
			}
			trials = append(trials, binary)
			mergeHarborMetadata(&metadata, trial)
		}
	}
	if len(trials) == 0 {
		return nil, HarborRunMetadata{}, errors.New("Harbor job contains no trial results")
	}
	if info.IsDir() {
		inspectHarborProtocol(root, paths, trials, &metadata)
	} else {
		metadata.ProtocolViolations = append(metadata.ProtocolViolations,
			"a standalone Harbor result cannot prove the pinned dataset and unmodified harness configuration",
		)
	}
	return trials, metadata, nil
}

// LoadHarborJobs combines independently resumable Harbor jobs into one score
// input. Terminal-Bench submissions may legitimately span multiple source
// jobs; protocol evidence is validated inside each job before any trials are
// merged. Duplicate trial IDs and mixed agent/model metadata are rejected as
// protocol violations instead of being silently averaged together.
func LoadHarborJobs(roots []string) ([]BinaryTrial, HarborRunMetadata, error) {
	if len(roots) == 0 {
		return nil, HarborRunMetadata{}, errors.New("at least one Harbor job is required")
	}
	combined := make([]BinaryTrial, 0)
	metadata := HarborRunMetadata{}
	seen := make(map[string]struct{})
	for _, root := range roots {
		trials, jobMetadata, err := LoadHarborJob(root)
		if err != nil {
			return nil, HarborRunMetadata{}, fmt.Errorf("load Harbor job %s: %w", root, err)
		}
		for _, trial := range trials {
			if trial.TrialID == "" {
				metadata.ProtocolViolations = append(metadata.ProtocolViolations, "Harbor result contains a trial without an ID")
			} else if _, exists := seen[trial.TrialID]; exists {
				metadata.ProtocolViolations = append(metadata.ProtocolViolations, fmt.Sprintf("duplicate Harbor trial ID %s", trial.TrialID))
				continue
			} else {
				seen[trial.TrialID] = struct{}{}
			}
			combined = append(combined, trial)
		}
		mergeHarborRunMetadata(&metadata, jobMetadata)
	}
	if len(combined) == 0 {
		return nil, HarborRunMetadata{}, errors.New("Harbor jobs contain no trial results")
	}
	return combined, metadata, nil
}

func mergeHarborRunMetadata(target *HarborRunMetadata, observed HarborRunMetadata) {
	mergeRecordedValue(&target.Agent, observed.Agent, "agent", target)
	mergeRecordedValue(&target.AgentVersion, observed.AgentVersion, "agent version", target)
	mergeRecordedValue(&target.Model, observed.Model, "model", target)
	mergeRecordedValue(&target.ReasoningEffort, observed.ReasoningEffort, "reasoning effort", target)
	mergeRecordedValue(&target.Source, observed.Source, "source", target)
	mergeRecordedValue(&target.DatasetRef, observed.DatasetRef, "dataset reference", target)
	mergeRecordedValue(&target.HarnessVersion, observed.HarnessVersion, "harness version", target)
	target.ProtocolViolations = append(target.ProtocolViolations, observed.ProtocolViolations...)
	target.ProtocolWarnings = append(target.ProtocolWarnings, observed.ProtocolWarnings...)
	if len(observed.ExceptionCounts) > 0 {
		if target.ExceptionCounts == nil {
			target.ExceptionCounts = make(map[string]int)
		}
		for name, count := range observed.ExceptionCounts {
			target.ExceptionCounts[name] += count
		}
	}
}

// ApplyHarborProtocolMetadata combines metric coverage with evidence from the
// Harbor config, lock file, and successful-trial ATIF artifacts.
func ApplyHarborProtocolMetadata(score *Scorecard, metadata HarborRunMetadata) {
	if score == nil {
		return
	}
	score.HarnessVersion = metadata.HarnessVersion
	score.Compliance.Violations = append(score.Compliance.Violations, metadata.ProtocolViolations...)
	score.Compliance.Warnings = append(score.Compliance.Warnings, metadata.ProtocolWarnings...)
	sort.Strings(score.Compliance.Violations)
	score.Compliance.Violations = uniqueStrings(score.Compliance.Violations)
	sort.Strings(score.Compliance.Warnings)
	score.Compliance.Warnings = uniqueStrings(score.Compliance.Warnings)
	score.Compliance.Comparable = len(score.Compliance.Violations) == 0
}

func inspectHarborProtocol(root string, resultPaths []string, trials []BinaryTrial, metadata *HarborRunMetadata) {
	configPath := filepath.Join(root, "config.json")
	payload, err := readBoundedJSON(configPath)
	if err != nil {
		metadata.ProtocolViolations = append(metadata.ProtocolViolations, "Harbor job config.json is unavailable")
	} else {
		var config harborJobConfig
		if json.Unmarshal(payload, &config) != nil || len(config.Datasets) != 1 {
			metadata.ProtocolViolations = append(metadata.ProtocolViolations, "Harbor job must contain exactly one pinned dataset")
		} else {
			metadata.DatasetRef = config.Datasets[0].Name + "@" + config.Datasets[0].Ref
			if metadata.DatasetRef != TerminalBench21DatasetRef {
				metadata.ProtocolViolations = append(metadata.ProtocolViolations,
					fmt.Sprintf("expected dataset %s, observed %s", TerminalBench21DatasetRef, metadata.DatasetRef),
				)
			}
			for _, agent := range config.Agents {
				mergeRecordedValue(&metadata.Model, agent.ModelName, "model", metadata)
				mergeRecordedValue(&metadata.ReasoningEffort, agent.Kwargs.ReasoningEffort, "reasoning effort", metadata)
			}
		}
	}

	lockPath := filepath.Join(root, "lock.json")
	payload, err = readBoundedJSON(lockPath)
	if err != nil {
		metadata.ProtocolViolations = append(metadata.ProtocolViolations, "Harbor job lock.json is unavailable")
	} else {
		var lock harborJobLock
		if err := json.Unmarshal(payload, &lock); err != nil {
			metadata.ProtocolViolations = append(metadata.ProtocolViolations, "Harbor job lock.json is invalid")
		} else {
			metadata.HarnessVersion = lock.Harbor.Version
			lockedTasks := make(map[string]struct {
				digest string
				source string
			})
			for _, trial := range lock.Trials {
				if trial.Task.Name == "" || trial.Task.Digest == "" {
					metadata.ProtocolViolations = append(metadata.ProtocolViolations, "Harbor job lock contains a task without name or digest")
				} else if previous, exists := lockedTasks[trial.Task.Name]; exists && (previous.digest != trial.Task.Digest || previous.source != trial.Task.Source) {
					metadata.ProtocolViolations = append(metadata.ProtocolViolations, fmt.Sprintf("task %s has inconsistent lock digests", trial.Task.Name))
				} else {
					lockedTasks[trial.Task.Name] = struct {
						digest string
						source string
					}{digest: trial.Task.Digest, source: trial.Task.Source}
				}
				mergeRecordedValue(&metadata.Model, trial.Agent.ModelName, "model", metadata)
				mergeRecordedValue(&metadata.ReasoningEffort, trial.Agent.Kwargs.ReasoningEffort, "reasoning effort", metadata)
				if trial.TimeoutMultiplier != 0 && trial.TimeoutMultiplier != 1 {
					metadata.ProtocolViolations = append(metadata.ProtocolViolations, "timeout_multiplier must be 1")
				}
				if trial.AgentTimeoutMultiplier != nil || trial.VerifierTimeoutMultiplier != nil || trial.SetupTimeoutMultiplier != nil || trial.EnvironmentTimeoutMultiplier != nil || trial.Agent.OverrideTimeoutSec != nil || trial.Agent.OverrideSetupTimeoutSec != nil || trial.Agent.MaxTimeoutSec != nil || trial.Verifier.OverrideTimeoutSec != nil || trial.Verifier.MaxTimeoutSec != nil {
					metadata.ProtocolViolations = append(metadata.ProtocolViolations, "agent, verifier, setup, and environment timeout overrides are not allowed")
				}
				if trial.Environment.OverrideCPUs != nil || trial.Environment.OverrideMemoryMB != nil || trial.Environment.OverrideStorageMB != nil || trial.Environment.OverrideGPUs != nil {
					metadata.ProtocolViolations = append(metadata.ProtocolViolations, "environment resource overrides are not allowed")
				}
				if trial.Verifier.Disable {
					metadata.ProtocolViolations = append(metadata.ProtocolViolations, "official verification must be enabled")
				}
			}
			if len(lock.Trials) != len(trials) {
				metadata.ProtocolViolations = append(metadata.ProtocolViolations,
					fmt.Sprintf("Harbor job lock contains %d trials but results contain %d", len(lock.Trials), len(trials)),
				)
			}
			for _, path := range resultPaths {
				resultPayload, readErr := readBoundedJSON(path)
				if readErr != nil {
					continue
				}
				var result harborTrialResult
				if json.Unmarshal(resultPayload, &result) != nil || result.TaskName == "" {
					continue
				}
				locked, exists := lockedTasks[result.TaskName]
				if !exists {
					metadata.ProtocolViolations = append(metadata.ProtocolViolations, fmt.Sprintf("result task %s is absent from the Harbor job lock", result.TaskName))
					continue
				}
				if result.TaskID.Ref != locked.digest {
					metadata.ProtocolViolations = append(metadata.ProtocolViolations, fmt.Sprintf("result task %s digest does not match the Harbor job lock", result.TaskName))
				}
				if result.Source != locked.source {
					metadata.ProtocolViolations = append(metadata.ProtocolViolations, fmt.Sprintf("result task %s source does not match the Harbor job lock", result.TaskName))
				}
			}
		}
	}

	successfulTraceCount := 0
	for _, path := range resultPaths {
		payload, readErr := readBoundedJSON(path)
		if readErr != nil {
			continue
		}
		var trial harborTrialResult
		if json.Unmarshal(payload, &trial) != nil || trial.TaskName == "" {
			continue
		}
		binary, convertErr := binaryTrialFromHarbor(trial)
		if convertErr != nil || !trialSucceeded(binary) {
			continue
		}
		trajectoryPath := filepath.Join(filepath.Dir(path), "agent", "trajectory.json")
		if _, statErr := os.Stat(trajectoryPath); statErr == nil {
			successfulTraceCount++
		} else {
			metadata.ProtocolViolations = append(metadata.ProtocolViolations,
				fmt.Sprintf("successful trial %s has no ATIF trajectory", trial.TaskName),
			)
		}
	}
	if successfulTraceCount > 0 {
		metadata.ProtocolWarnings = append(metadata.ProtocolWarnings,
			fmt.Sprintf("%d successful-trial ATIF trajectories are present but have not undergone the official reward-hacking audit", successfulTraceCount),
		)
	}
}

func uniqueStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	output := values[:0]
	for _, value := range values {
		if len(output) == 0 || output[len(output)-1] != value {
			output = append(output, value)
		}
	}
	return output
}

func binaryTrialFromHarbor(trial harborTrialResult) (BinaryTrial, error) {
	if strings.TrimSpace(trial.TaskName) == "" {
		return BinaryTrial{}, errors.New("task_name is required")
	}
	var reward *float64
	if trial.VerifierResult != nil && trial.VerifierResult.Rewards != nil {
		value, exists := trial.VerifierResult.Rewards["reward"]
		if !exists && len(trial.VerifierResult.Rewards) == 1 {
			for _, candidate := range trial.VerifierResult.Rewards {
				value, exists = candidate, true
			}
		}
		if exists {
			parsed, err := value.Float64()
			if err != nil {
				return BinaryTrial{}, fmt.Errorf("invalid reward: %w", err)
			}
			reward = &parsed
		}
	}
	errored := len(trial.ExceptionInfo) > 0 && string(trial.ExceptionInfo) != "null"
	return BinaryTrial{TrialID: trial.ID, TaskID: trial.TaskName, Reward: reward, Errored: errored}, nil
}

// ApplyTerminalBenchAudit applies the official reward-hacking judge output.
// The accepted JSON is either a leaderboard submission object containing
// disqualified_trials or a plain array of trial IDs. Unknown IDs are rejected
// so an audit from a different run cannot silently alter the score.
func ApplyTerminalBenchAudit(trials []BinaryTrial, path string) (int, error) {
	payload, err := readBoundedJSON(path)
	if err != nil {
		return 0, err
	}
	var audit terminalBenchAudit
	identifiers := make([]string, 0)
	if err := json.Unmarshal(payload, &audit); err == nil && audit.DisqualifiedTrials != nil {
		identifiers = make([]string, 0, len(*audit.DisqualifiedTrials))
		for _, item := range *audit.DisqualifiedTrials {
			identifiers = append(identifiers, item.TrialID)
		}
	} else {
		if err := json.Unmarshal(payload, &identifiers); err != nil {
			return 0, errors.New("Terminal-Bench audit has no disqualified_trials")
		}
	}
	wanted := make(map[string]struct{}, len(identifiers))
	for _, identifier := range identifiers {
		identifier = strings.TrimSpace(identifier)
		if identifier == "" {
			return 0, errors.New("Terminal-Bench audit contains an empty trial ID")
		}
		wanted[identifier] = struct{}{}
	}
	for index := range trials {
		if _, exists := wanted[trials[index].TrialID]; exists {
			trials[index].Disqualified = true
			delete(wanted, trials[index].TrialID)
		}
	}
	if len(wanted) > 0 {
		unknown := make([]string, 0, len(wanted))
		for identifier := range wanted {
			unknown = append(unknown, identifier)
		}
		sort.Strings(unknown)
		return 0, fmt.Errorf("Terminal-Bench audit references %d unknown trial IDs (first: %s)", len(unknown), unknown[0])
	}
	return len(identifiers), nil
}

func mergeHarborMetadata(metadata *HarborRunMetadata, trial harborTrialResult) {
	mergeRecordedValue(&metadata.Agent, trial.AgentInfo.Name, "agent", metadata)
	mergeRecordedValue(&metadata.AgentVersion, trial.AgentInfo.Version, "agent version", metadata)
	mergeRecordedValue(&metadata.Source, trial.Source, "source", metadata)
	if trial.AgentInfo.ModelInfo != nil {
		model := trial.AgentInfo.ModelInfo.Name
		if trial.AgentInfo.ModelInfo.Provider != "" && !strings.Contains(model, "/") {
			model = trial.AgentInfo.ModelInfo.Provider + "/" + model
		}
		mergeRecordedValue(&metadata.Model, model, "model", metadata)
	}
	if len(trial.ExceptionInfo) > 0 && string(trial.ExceptionInfo) != "null" {
		var exception struct {
			Type string `json:"exception_type"`
		}
		if json.Unmarshal(trial.ExceptionInfo, &exception) == nil && exception.Type != "" {
			if metadata.ExceptionCounts == nil {
				metadata.ExceptionCounts = make(map[string]int)
			}
			metadata.ExceptionCounts[exception.Type]++
		}
	}
}

func mergeRecordedValue(target *string, observed, label string, metadata *HarborRunMetadata) {
	observed = strings.TrimSpace(observed)
	if observed == "" {
		return
	}
	if *target == "" {
		*target = observed
		return
	}
	if *target != observed {
		metadata.ProtocolViolations = append(metadata.ProtocolViolations,
			fmt.Sprintf("Harbor results contain multiple %s values: %s and %s", label, *target, observed),
		)
	}
}

type SWEBenchRunReport struct {
	TotalInstances            int      `json:"total_instances"`
	SubmittedInstances        int      `json:"submitted_instances"`
	CompletedInstances        int      `json:"completed_instances"`
	ResolvedInstances         int      `json:"resolved_instances"`
	UnresolvedInstances       int      `json:"unresolved_instances"`
	InfraFailureInstances     int      `json:"infra_failure_instances"`
	AmbiguousFailureInstances int      `json:"ambiguous_failure_instances"`
	EmptyPatchInstances       int      `json:"empty_patch_instances"`
	ErrorInstances            int      `json:"error_instances"`
	SchemaVersion             int      `json:"schema_version"`
	ResolvedIDs               []string `json:"resolved_ids"`
}

func LoadSWEBenchRunReport(path string) (SWEBenchRunReport, error) {
	payload, err := readBoundedJSON(path)
	if err != nil {
		return SWEBenchRunReport{}, err
	}
	var report SWEBenchRunReport
	if err := json.Unmarshal(payload, &report); err != nil {
		return SWEBenchRunReport{}, fmt.Errorf("decode SWE-bench run report: %w", err)
	}
	if report.TotalInstances <= 0 {
		return SWEBenchRunReport{}, errors.New("SWE-bench run report has no total_instances")
	}
	if report.ResolvedInstances == 0 && len(report.ResolvedIDs) > 0 {
		report.ResolvedInstances = len(report.ResolvedIDs)
	}
	return report, nil
}

func ScoreSWEBenchVerified(report SWEBenchRunReport) Scorecard {
	protocol, _ := ProtocolForBenchmark(BenchmarkSWEBenchVerified)
	denominator := report.TotalInstances
	value := 0.0
	if denominator > 0 {
		value = roundTo(100*float64(report.ResolvedInstances)/float64(denominator), 2)
	}
	violations := make([]string, 0, 2)
	if report.TotalInstances != protocol.ExpectedTasks {
		violations = append(violations, fmt.Sprintf("expected %d tasks, report contains %d", protocol.ExpectedTasks, report.TotalInstances))
	}
	if report.SubmittedInstances != protocol.ExpectedTasks {
		violations = append(violations, fmt.Sprintf("expected predictions for %d tasks, observed %d", protocol.ExpectedTasks, report.SubmittedInstances))
	}
	warnings := make([]string, 0, 2)
	if report.InfraFailureInstances > 0 {
		warnings = append(warnings, fmt.Sprintf("%d instances were classified as likely infrastructure failures and still count as failures", report.InfraFailureInstances))
	}
	if report.AmbiguousFailureInstances > 0 {
		warnings = append(warnings, fmt.Sprintf("%d instances had ambiguous failures and still count as failures", report.AmbiguousFailureInstances))
	}
	return Scorecard{
		Schema: ScorecardSchema, Protocol: protocol,
		PrimaryMetric: ScoreMetric{
			Name: protocol.PrimaryMetric, ValuePercent: value,
			Successful: report.ResolvedInstances, Total: denominator,
		},
		Coverage: ScoreCoverage{
			ExpectedTasks: protocol.ExpectedTasks, ObservedTasks: report.TotalInstances,
			MinimumAttemptsPerTask: 1, MaximumAttemptsPerTask: 1,
			Trials: denominator, SuccessfulTrials: report.ResolvedInstances,
			FailedTrials:  denominator - report.ResolvedInstances,
			ErroredTrials: report.ErrorInstances,
		},
		Compliance: ProtocolCompliance{
			Comparable: len(violations) == 0,
			Violations: violations,
			Warnings: append(warnings,
				"local score uses the official results.json denominator; official publication remains subject to SWE-bench submission review",
			),
		},
		Metadata: map[string]any{
			"schema_version":        report.SchemaVersion,
			"completed_instances":   report.CompletedInstances,
			"unresolved_instances":  report.UnresolvedInstances,
			"empty_patch_instances": report.EmptyPatchInstances,
		},
	}
}

func readBoundedJSON(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	payload, err := io.ReadAll(io.LimitReader(file, maximumOfficialResultBytes+1))
	if err != nil {
		return nil, err
	}
	if len(payload) > maximumOfficialResultBytes {
		return nil, fmt.Errorf("official result exceeds %d bytes: %s", maximumOfficialResultBytes, path)
	}
	return payload, nil
}

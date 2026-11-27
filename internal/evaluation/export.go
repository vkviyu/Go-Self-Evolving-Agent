package evaluation

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// SWEPrediction is the prediction protocol accepted by the official
// SWE-bench evaluation harness.
type SWEPrediction struct {
	InstanceID      string `json:"instance_id"`
	ModelNameOrPath string `json:"model_name_or_path"`
	ModelPatch      string `json:"model_patch"`
}

func WriteSWEPredictions(writer io.Writer, results []Result, modelOverride string) error {
	if writer == nil {
		return errors.New("SWE-bench prediction writer is required")
	}
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	for _, result := range results {
		instanceID := strings.TrimSpace(result.InstanceID)
		if instanceID == "" {
			instanceID = strings.TrimSpace(result.TaskID)
		}
		modelName := strings.TrimSpace(modelOverride)
		if modelName == "" {
			modelName = strings.TrimSpace(result.Model)
		}
		if instanceID == "" || modelName == "" {
			return errors.New("SWE-bench predictions require instance and model names")
		}
		if err := encoder.Encode(SWEPrediction{
			InstanceID: instanceID, ModelNameOrPath: modelName, ModelPatch: result.Patch,
		}); err != nil {
			return err
		}
	}
	return nil
}

type Summary struct {
	RunID            string                   `json:"run_id,omitempty"`
	Tasks            int                      `json:"attempts"`
	UniqueTasks      int                      `json:"unique_tasks"`
	Resolved         int                      `json:"resolved_attempts"`
	Failed           int                      `json:"unresolved_attempts"`
	ResolvePct       float64                  `json:"resolve_percent"`
	TasksWithSuccess int                      `json:"tasks_with_success"`
	ObservedPassAtK  float64                  `json:"observed_pass_at_k_percent"`
	MinimumAttempts  int                      `json:"minimum_attempts_per_task"`
	MaximumAttempts  int                      `json:"maximum_attempts_per_task"`
	AgentFailures    int                      `json:"agent_failures"`
	AgentTimeouts    int                      `json:"agent_timeouts"`
	Checks           int                      `json:"checks"`
	CheckFailures    int                      `json:"check_failures"`
	DurationMS       int64                    `json:"duration_ms"`
	Usage            Usage                    `json:"usage"`
	BySuite          map[string]SummaryBucket `json:"by_suite"`
	ByLanguage       map[string]SummaryBucket `json:"by_language"`
}

type SummaryBucket struct {
	Attempts int `json:"attempts"`
	Resolved int `json:"resolved"`
}

// Summarize aggregates recorded outcomes by suite and language and computes task success
// across attempts.
func Summarize(results []Result) Summary {
	summary := Summary{
		Tasks: len(results), Failed: len(results),
		BySuite: make(map[string]SummaryBucket), ByLanguage: make(map[string]SummaryBucket),
	}
	taskAttempts := make(map[string]int)
	taskSucceeded := make(map[string]bool)
	for _, result := range results {
		if summary.RunID == "" {
			summary.RunID = result.RunID
		} else if summary.RunID != result.RunID {
			summary.RunID = ""
		}
		taskAttempts[result.TaskID]++
		summary.DurationMS += result.DurationMS
		summary.Usage.Input += result.Agent.Usage.Input
		summary.Usage.Cached += result.Agent.Usage.Cached
		summary.Usage.Output += result.Agent.Usage.Output
		summary.Usage.Reasoning += result.Agent.Usage.Reasoning
		summary.Usage.Total += result.Agent.Usage.Total
		summary.Usage.Records += result.Agent.Usage.Records
		summary.Usage.CostMicros += result.Agent.Usage.CostMicros
		if result.Agent.Status == "failed" {
			summary.AgentFailures++
		}
		if result.Agent.Status == "timed_out" {
			summary.AgentTimeouts++
		}
		for _, check := range result.Checks {
			summary.Checks++
			if !check.Passed {
				summary.CheckFailures++
			}
		}
		suite := summary.BySuite[result.Suite]
		suite.Attempts++
		language := summary.ByLanguage[result.Language]
		language.Attempts++
		if result.Resolved {
			summary.Resolved++
			summary.Failed--
			taskSucceeded[result.TaskID] = true
			suite.Resolved++
			language.Resolved++
		}
		summary.BySuite[result.Suite] = suite
		summary.ByLanguage[result.Language] = language
	}
	if summary.Tasks > 0 {
		summary.ResolvePct = 100 * float64(summary.Resolved) / float64(summary.Tasks)
	}
	summary.UniqueTasks = len(taskAttempts)
	summary.MinimumAttempts = 0
	for taskID, attempts := range taskAttempts {
		if summary.MinimumAttempts == 0 || attempts < summary.MinimumAttempts {
			summary.MinimumAttempts = attempts
		}
		if attempts > summary.MaximumAttempts {
			summary.MaximumAttempts = attempts
		}
		if taskSucceeded[taskID] {
			summary.TasksWithSuccess++
		}
	}
	if summary.UniqueTasks > 0 {
		summary.ObservedPassAtK = 100 * float64(summary.TasksWithSuccess) / float64(summary.UniqueTasks)
	}
	return summary
}

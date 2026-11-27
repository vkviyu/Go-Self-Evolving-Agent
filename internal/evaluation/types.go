// Package evaluation defines the language-neutral benchmark task, execution
// and result protocol used to evaluate GSEA as a black-box coding Agent.
package evaluation

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	TaskSchema   = "gsea.eval.task.v1"
	ResultSchema = "gsea.eval.result.v1"

	BackendSandbox = "sandbox"
	BackendDocker  = "docker"

	WorkspaceGitWorktree = "git_worktree"
	WorkspaceDirectory   = "directory_copy"

	PhaseTarget     = "target"
	PhaseRegression = "regression"
	PhaseSafety     = "safety"
	PhaseGeneral    = "general"
)

var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,255}$`)

// Task describes one repository state, user-visible problem and evaluator-only
// checks. Checks are never included in ProblemStatement when the Agent runs.
type Task struct {
	Schema           string            `json:"schema"`
	ID               string            `json:"id"`
	InstanceID       string            `json:"instance_id,omitempty"`
	Suite            string            `json:"suite"`
	Kind             string            `json:"kind"`
	Language         string            `json:"language,omitempty"`
	WorkspaceMode    string            `json:"workspace_mode,omitempty"`
	Repository       string            `json:"repository"`
	BaseCommit       string            `json:"base_commit,omitempty"`
	ProblemStatement string            `json:"problem_statement"`
	AgentTimeoutSecs int               `json:"agent_timeout_seconds,omitempty"`
	Checks           []Check           `json:"checks"`
	Tags             []string          `json:"tags,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

// Check is an evaluator-controlled verifier. Sandbox runs on the host through
// GSEA's OS sandbox with network disabled. Docker runs in the named image with
// the task worktree mounted at /workspace and network disabled.
type Check struct {
	Name           string `json:"name"`
	Phase          string `json:"phase,omitempty"`
	Backend        string `json:"backend,omitempty"`
	Command        string `json:"command"`
	Image          string `json:"image,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	Optional       bool   `json:"optional,omitempty"`
}

type Usage struct {
	Input      int   `json:"input_tokens,omitempty"`
	Cached     int   `json:"cached_tokens,omitempty"`
	Output     int   `json:"output_tokens,omitempty"`
	Reasoning  int   `json:"reasoning_tokens,omitempty"`
	Total      int   `json:"total_tokens,omitempty"`
	Records    int   `json:"records,omitempty"`
	CostMicros int64 `json:"cost_micros,omitempty"`
}

// AgentRequest is the deliberately narrow view passed to the Agent adapter.
// Repository baselines and hidden verifier commands remain evaluator-owned.
type AgentRequest struct {
	TaskID           string
	Suite            string
	Kind             string
	Language         string
	ProblemStatement string
}

type AgentOutcome struct {
	Status         string `json:"status"`
	SessionID      string `json:"session_id,omitempty"`
	TranscriptPath string `json:"transcript_path,omitempty"`
	StderrPath     string `json:"stderr_path,omitempty"`
	Error          string `json:"error,omitempty"`
	Usage          Usage  `json:"usage"`
}

type CheckResult struct {
	Name       string `json:"name"`
	Phase      string `json:"phase"`
	Backend    string `json:"backend"`
	Command    string `json:"command"`
	Image      string `json:"image,omitempty"`
	Optional   bool   `json:"optional,omitempty"`
	Passed     bool   `json:"passed"`
	ExitCode   int    `json:"exit_code"`
	TimedOut   bool   `json:"timed_out,omitempty"`
	Truncated  bool   `json:"output_truncated,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	OutputPath string `json:"output_path,omitempty"`
	Error      string `json:"error,omitempty"`
}

// Result is one durable, self-contained benchmark outcome. Patch is embedded
// for compatibility with SWE-bench prediction files; verbose logs live under
// RunDirectory and are referenced by path.
type Result struct {
	Schema        string            `json:"schema"`
	RunID         string            `json:"run_id"`
	TaskID        string            `json:"task_id"`
	InstanceID    string            `json:"instance_id,omitempty"`
	Suite         string            `json:"suite"`
	Kind          string            `json:"kind"`
	Language      string            `json:"language,omitempty"`
	WorkspaceMode string            `json:"workspace_mode,omitempty"`
	Model         string            `json:"model"`
	Agent         AgentOutcome      `json:"agent"`
	Checks        []CheckResult     `json:"checks"`
	Resolved      bool              `json:"resolved"`
	Patch         string            `json:"patch"`
	PatchSHA256   string            `json:"patch_sha256,omitempty"`
	PatchPath     string            `json:"patch_path,omitempty"`
	WorkspacePath string            `json:"workspace_path,omitempty"`
	RunDirectory  string            `json:"run_directory"`
	StartedAt     time.Time         `json:"started_at"`
	CompletedAt   time.Time         `json:"completed_at"`
	DurationMS    int64             `json:"duration_ms"`
	Error         string            `json:"error,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}

// Validate checks the task schema, identity and evaluation inputs before the runner uses
// them.
func (task Task) Validate() error {
	if task.Schema != TaskSchema {
		return fmt.Errorf("task schema must be %q", TaskSchema)
	}
	if !identifierPattern.MatchString(task.ID) {
		return errors.New("task id contains unsupported characters")
	}
	if strings.TrimSpace(task.Suite) == "" || strings.TrimSpace(task.Kind) == "" {
		return errors.New("task suite and kind are required")
	}
	if strings.TrimSpace(task.Repository) == "" {
		return errors.New("task repository is required")
	}
	switch mode := normalizedWorkspaceMode(task); mode {
	case WorkspaceGitWorktree:
		if strings.TrimSpace(task.BaseCommit) == "" {
			return errors.New("git_worktree task base_commit is required")
		}
		if strings.HasPrefix(strings.TrimSpace(task.BaseCommit), "-") {
			return errors.New("task base_commit must not begin with '-'")
		}
	case WorkspaceDirectory:
		if strings.TrimSpace(task.BaseCommit) != "" {
			return errors.New("directory_copy task must not set base_commit")
		}
	default:
		return fmt.Errorf("unsupported workspace_mode %q", task.WorkspaceMode)
	}
	if strings.TrimSpace(task.ProblemStatement) == "" {
		return errors.New("task problem_statement is required")
	}
	if task.AgentTimeoutSecs < 0 || task.AgentTimeoutSecs > 7200 {
		return errors.New("task agent_timeout_seconds must be between 0 and 7200")
	}
	if len(task.Checks) == 0 {
		return errors.New("task requires at least one evaluator check")
	}
	seen := make(map[string]struct{}, len(task.Checks))
	requiredChecks := 0
	for index, check := range task.Checks {
		if err := check.validate(); err != nil {
			return fmt.Errorf("check %d: %w", index, err)
		}
		if _, exists := seen[check.Name]; exists {
			return fmt.Errorf("duplicate check name %q", check.Name)
		}
		seen[check.Name] = struct{}{}
		if !check.Optional {
			requiredChecks++
		}
	}
	if requiredChecks == 0 {
		return errors.New("task requires at least one non-optional evaluator check")
	}
	return nil
}

func normalizedWorkspaceMode(task Task) string {
	if task.WorkspaceMode == "" {
		return WorkspaceGitWorktree
	}
	return task.WorkspaceMode
}

func (check Check) validate() error {
	if strings.TrimSpace(check.Name) == "" || strings.TrimSpace(check.Command) == "" {
		return errors.New("name and command are required")
	}
	backend := check.Backend
	if backend == "" {
		backend = BackendSandbox
	}
	if backend != BackendSandbox && backend != BackendDocker {
		return fmt.Errorf("unsupported backend %q", check.Backend)
	}
	if backend == BackendDocker && strings.TrimSpace(check.Image) == "" {
		return errors.New("docker check requires image")
	}
	if check.TimeoutSeconds < 0 || check.TimeoutSeconds > 7200 {
		return errors.New("timeout_seconds must be between 0 and 7200")
	}
	switch check.Phase {
	case "", PhaseTarget, PhaseRegression, PhaseSafety, PhaseGeneral:
		return nil
	default:
		return fmt.Errorf("unsupported phase %q", check.Phase)
	}
}

func normalizedBackend(check Check) string {
	if check.Backend == "" {
		return BackendSandbox
	}
	return check.Backend
}

func normalizedPhase(check Check) string {
	if check.Phase == "" {
		return PhaseGeneral
	}
	return check.Phase
}

package evaluation

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const defaultAgentTimeout = 30 * time.Minute

type AgentFunc func(context.Context, AgentRequest, string, string) (AgentOutcome, error)
type VerifierFunc func(context.Context, string, string, []Check) ([]CheckResult, error)

type RunnerConfig struct {
	Root          string
	RunID         string
	Model         string
	KeepWorktrees bool
	Agent         AgentFunc
	Verifier      VerifierFunc
	Now           func() time.Time
}

type Runner struct {
	root          string
	runID         string
	model         string
	keepWorktrees bool
	agent         AgentFunc
	verifier      VerifierFunc
	now           func() time.Time
}

func NewRunner(config RunnerConfig) (*Runner, error) {
	if strings.TrimSpace(config.Root) == "" || strings.TrimSpace(config.RunID) == "" {
		return nil, errors.New("evaluation root and run ID are required")
	}
	if config.Agent == nil {
		return nil, errors.New("evaluation Agent runner is required")
	}
	root, err := filepath.Abs(config.Root)
	if err != nil {
		return nil, fmt.Errorf("resolve evaluation root: %w", err)
	}
	if filepath.Dir(root) == root {
		return nil, errors.New("evaluation root must not be a filesystem root")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create evaluation root: %w", err)
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &Runner{
		root: root, runID: config.RunID, model: config.Model,
		keepWorktrees: config.KeepWorktrees, agent: config.Agent, verifier: config.Verifier, now: now,
	}, nil
}

func (runner *Runner) RunTask(ctx context.Context, task Task) (result Result) {
	started := runner.now().UTC()
	instanceID := task.InstanceID
	if instanceID == "" {
		instanceID = task.ID
	}
	result = Result{
		Schema: ResultSchema, RunID: runner.runID, TaskID: task.ID, InstanceID: instanceID,
		Suite: task.Suite, Kind: task.Kind, Language: task.Language,
		WorkspaceMode: normalizedWorkspaceMode(task), Model: runner.model,
		StartedAt: started, Metadata: cloneMetadata(task.Metadata),
	}
	finish := func() {
		result.CompletedAt = runner.now().UTC()
		result.DurationMS = result.CompletedAt.Sub(result.StartedAt).Milliseconds()
	}
	defer finish()
	if err := task.Validate(); err != nil {
		result.Error = err.Error()
		return result
	}
	runDirectory := filepath.Join(runner.root, "runs", safePathComponent(runner.runID), safePathComponent(task.ID))
	result.RunDirectory = runDirectory
	if err := validateTaskArtifactPath(task, runDirectory); err != nil {
		result.Error = err.Error()
		return result
	}
	if err := os.MkdirAll(runDirectory, 0o700); err != nil {
		result.Error = fmt.Sprintf("create task run directory: %v", err)
		return result
	}
	worktreeRoot := filepath.Join(runner.root, "worktree-runs", safePathComponent(runner.runID))
	worktree, err := PrepareWorkspace(ctx, worktreeRoot, task)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	if runner.keepWorktrees {
		result.WorkspacePath = worktree.Path
	}
	defer func() {
		if runner.keepWorktrees {
			return
		}
		cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if cleanupErr := worktree.Remove(cleanupContext); cleanupErr != nil {
			result.Error = joinMessage(result.Error, cleanupErr.Error())
		}
	}()

	agentTimeout := defaultAgentTimeout
	if task.AgentTimeoutSecs > 0 {
		agentTimeout = time.Duration(task.AgentTimeoutSecs) * time.Second
	}
	agentContext, cancelAgent := context.WithTimeout(ctx, agentTimeout)
	agentOutcome, agentErr := runner.agent(agentContext, AgentRequest{
		TaskID: task.ID, Suite: task.Suite, Kind: task.Kind,
		Language: task.Language, ProblemStatement: task.ProblemStatement,
	}, worktree.Path, runDirectory)
	cancelAgent()
	if agentOutcome.Status == "" {
		agentOutcome.Status = "completed"
	}
	if agentErr != nil {
		agentOutcome.Status = "failed"
		agentOutcome.Error = joinMessage(agentOutcome.Error, agentErr.Error())
	}
	if errors.Is(agentContext.Err(), context.DeadlineExceeded) {
		agentOutcome.Status = "timed_out"
	}
	result.Agent = agentOutcome

	patchContext, cancelPatch := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	patch, patchErr := worktree.Patch(patchContext)
	cancelPatch()
	if patchErr != nil {
		result.Error = joinMessage(result.Error, patchErr.Error())
	} else {
		result.Patch = string(patch)
		result.PatchSHA256 = PatchSHA256(patch)
		patchPath := filepath.Join(runDirectory, "model.patch")
		if err := os.WriteFile(patchPath, patch, 0o600); err != nil {
			result.Error = joinMessage(result.Error, fmt.Sprintf("write model patch: %v", err))
		} else {
			result.PatchPath = patchPath
		}
	}

	verifier := runner.verifier
	if verifier == nil {
		verifier = RunChecks
	}
	checks, checkErr := verifier(ctx, worktree.Path, runDirectory, task.Checks)
	result.Checks = checks
	if checkErr != nil {
		result.Error = joinMessage(result.Error, checkErr.Error())
	}
	result.Resolved = agentOutcome.Status == "completed" && agentErr == nil &&
		patchErr == nil && checkErr == nil && ChecksResolved(checks)
	return result
}

func validateTaskArtifactPath(task Task, runDirectory string) error {
	source, err := canonicalSourceDirectory(task.Repository)
	if err != nil {
		return err
	}
	prospective, err := canonicalProspectivePath(runDirectory)
	if err != nil {
		return fmt.Errorf("resolve task artifact directory: %w", err)
	}
	if pathContains(source, prospective) {
		return errors.New("evaluation artifact directory must not be inside the source fixture")
	}
	return nil
}

func cloneMetadata(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	clone := make(map[string]string, len(source))
	maps.Copy(clone, source)
	return clone
}

// joinMessage combines nonempty evaluation diagnostics without adding a separator for an
// absent message.
func joinMessage(first, second string) string {
	first, second = strings.TrimSpace(first), strings.TrimSpace(second)
	if first == "" {
		return second
	}
	if second == "" {
		return first
	}
	return first + "; " + second
}

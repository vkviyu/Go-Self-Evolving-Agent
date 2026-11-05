package evaluation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/sandbox"
)

const (
	defaultCheckTimeout = 10 * time.Minute
	maximumCheckOutput  = 2 << 20
)

var dockerImagePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]{0,511}$`)

func RunChecks(ctx context.Context, workspace, runDirectory string, checks []Check) ([]CheckResult, error) {
	if strings.TrimSpace(workspace) == "" || strings.TrimSpace(runDirectory) == "" {
		return nil, errors.New("workspace and run directory are required")
	}
	if err := os.MkdirAll(runDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("create check output directory: %w", err)
	}
	results := make([]CheckResult, 0, len(checks))
	for index, check := range checks {
		result := runCheck(ctx, workspace, runDirectory, index, check)
		results = append(results, result)
		if ctx.Err() != nil {
			return results, ctx.Err()
		}
	}
	return results, nil
}

func runCheck(ctx context.Context, workspace, runDirectory string, index int, check Check) CheckResult {
	backend := normalizedBackend(check)
	result := CheckResult{
		Name: check.Name, Phase: normalizedPhase(check), Backend: backend,
		Command: check.Command, Image: check.Image, Optional: check.Optional, ExitCode: -1,
	}
	timeout := defaultCheckTimeout
	if check.TimeoutSeconds > 0 {
		timeout = time.Duration(check.TimeoutSeconds) * time.Second
	}
	checkContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	started := time.Now()
	var output cappedBuffer
	output.limit = maximumCheckOutput
	var err error
	switch backend {
	case BackendSandbox:
		var runner sandbox.Runner
		runner, err = sandbox.New(workspace)
		if err == nil {
			err = runner.Run(checkContext, sandbox.Request{Command: check.Command, WorkingDirectory: workspace}, &output, &output)
		}
	case BackendDocker:
		err = runDockerCheck(checkContext, workspace, check, &output)
	default:
		err = fmt.Errorf("unsupported verifier backend %q", backend)
	}
	result.DurationMS = time.Since(started).Milliseconds()
	result.TimedOut = errors.Is(checkContext.Err(), context.DeadlineExceeded)
	result.ExitCode = commandExitCode(err)
	result.Passed = err == nil && !result.TimedOut
	result.Truncated = output.truncated
	if err != nil {
		result.Error = err.Error()
	}
	outputPath := filepath.Join(runDirectory, fmt.Sprintf("check-%03d-%s.log", index+1, safePathComponent(check.Name)))
	if writeErr := os.WriteFile(outputPath, output.Bytes(), 0o600); writeErr != nil {
		if result.Error == "" {
			result.Error = writeErr.Error()
		} else {
			result.Error += "; write output: " + writeErr.Error()
		}
		result.Passed = false
	} else {
		result.OutputPath = outputPath
	}
	return result
}

func runDockerCheck(ctx context.Context, workspace string, check Check, output *cappedBuffer) error {
	arguments, err := dockerCheckArguments(workspace, check)
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, "docker", arguments...)
	command.Stdout = output
	command.Stderr = output
	return command.Run()
}

func dockerCheckArguments(workspace string, check Check) ([]string, error) {
	image := strings.TrimSpace(check.Image)
	if !dockerImagePattern.MatchString(image) {
		return nil, errors.New("docker image contains unsupported characters")
	}
	absolute, err := filepath.Abs(workspace)
	if err != nil {
		return nil, fmt.Errorf("resolve docker workspace: %w", err)
	}
	return []string{
		"run", "--rm", "--pull", "never", "--network", "none",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "512",
		"--volume", absolute + ":/workspace", "--workdir", "/workspace",
		"--entrypoint", "/bin/sh", image, "-lc", check.Command,
	}, nil
}

func commandExitCode(err error) int {
	if err == nil {
		return 0
	}
	if exitError, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitError.ExitCode()
	}
	return -1
}

func ChecksResolved(results []CheckResult) bool {
	if len(results) == 0 {
		return false
	}
	for _, result := range results {
		if !result.Optional && !result.Passed {
			return false
		}
	}
	return true
}

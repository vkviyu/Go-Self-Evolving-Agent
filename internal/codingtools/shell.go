package codingtools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/sandbox"
)

type shellArgs struct {
	Command          string `json:"command"`
	WorkingDirectory string `json:"working_directory"`
	TimeoutMS        int    `json:"timeout_ms"`
	MaxOutputBytes   int    `json:"max_output_bytes"`
}

func (workspace *Workspace) shell(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	return workspace.withWorkspaceLease(ctx, func() (json.RawMessage, error) {
		return workspace.shellUnlocked(ctx, raw)
	})
}

func (workspace *Workspace) shellUnlocked(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var args shellArgs
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.Command) == "" {
		return nil, errors.New("command must not be empty")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	workingDirectory, _, err := workspace.operationRoot(ctx)
	if err != nil {
		return nil, err
	}
	runner, err := workspace.runnerFor(workingDirectory)
	if err != nil {
		return nil, err
	}
	operationRoot := workingDirectory
	if strings.TrimSpace(args.WorkingDirectory) != "" {
		workingDirectory, _, err = workspace.resolveExistingAt(workingDirectory, args.WorkingDirectory)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(workingDirectory)
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("working_directory is not a directory")
		}
	}
	limit, err := bounded(args.MaxOutputBytes, defaultShellBytes, maximumShellBytes)
	if err != nil {
		return nil, err
	}
	if args.TimeoutMS < 0 || args.TimeoutMS > 300000 {
		return nil, errors.New("timeout_ms must be between 1 and 300000 when supplied")
	}
	before, err := snapshotWorkspace(ctx, operationRoot)
	if err != nil {
		return nil, fmt.Errorf("capture workspace before shell execution: %w", err)
	}
	finalize := func(result map[string]any) (json.RawMessage, error) {
		reportContext := context.WithoutCancel(ctx)
		after := snapshotAfterMutation(reportContext, operationRoot)
		capture := captureWorkspaceChanges(reportContext, workspace.artifacts, before, after)
		result["changes"] = capture.changes
		appendDiffs(reportContext, workspace.artifacts, result, capture.diffs, capture.diffsTruncated)
		if capture.truncated {
			result["changes_truncated"] = true
			result["changes_message"] = capture.message
		}
		return json.Marshal(result)
	}
	var stdout, stderr limitedBuffer
	stdout.limit, stderr.limit = limit, limit
	var inherited []*os.File
	if workspace.liveness != nil {
		// Descendants that escape the process group keep this runtime's
		// leases alive until they exit, even if the runtime itself dies.
		token, err := workspace.liveness.DescendantLock()
		if err != nil {
			return nil, fmt.Errorf("register shell process liveness: %w", err)
		}
		defer token.Close()
		inherited = []*os.File{token}
	}
	commandContext := ctx
	if args.TimeoutMS > 0 {
		var cancel context.CancelFunc
		commandContext, cancel = context.WithTimeout(ctx, time.Duration(args.TimeoutMS)*time.Millisecond)
		defer cancel()
	}
	err = runner.Run(commandContext, sandbox.Request{
		Command:          args.Command,
		WorkingDirectory: workingDirectory,
		WorkspaceRoot:    operationRoot,
		InheritedFiles:   inherited,
	}, &stdout, &stderr)
	if err != nil && commandContext.Err() != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return finalize(map[string]any{"command": args.Command, "working_directory": relativePath(operationRoot, workingDirectory), "stdout": stdout.String(), "stderr": stderr.String(), "exit_code": -1, "timed_out": errors.Is(commandContext.Err(), context.DeadlineExceeded), "output_truncated": stdout.truncated || stderr.truncated})
	}
	exitCode := 0
	if err != nil {
		if exitError, ok := errors.AsType[*exec.ExitError](err); ok {
			exitCode = exitError.ExitCode()
		} else {
			return nil, err
		}
	}
	return finalize(map[string]any{"command": args.Command, "working_directory": relativePath(operationRoot, workingDirectory), "stdout": stdout.String(), "stderr": stderr.String(), "exit_code": exitCode, "timed_out": false, "output_truncated": stdout.truncated || stderr.truncated})
}

type limitedBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (buffer *limitedBuffer) Write(data []byte) (int, error) {
	remaining := buffer.limit - buffer.Len()
	if remaining <= 0 {
		buffer.truncated = true
		return len(data), nil
	}
	if len(data) > remaining {
		_, _ = buffer.Buffer.Write(data[:remaining])
		buffer.truncated = true
		return len(data), nil
	}
	return buffer.Buffer.Write(data)
}

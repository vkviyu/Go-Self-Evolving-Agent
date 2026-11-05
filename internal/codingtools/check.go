package codingtools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/sandbox"
)

// checkOutputBytes bounds the output a failed check reports.
const checkOutputBytes = 4 << 10

// RunCheck runs a host-configured command in the sandbox of the main
// workspace and fails when it exits nonzero or times out. It takes no lease
// and records no changes, so the command should only read the workspace.
func (workspace *Workspace) RunCheck(ctx context.Context, command string, timeout time.Duration) error {
	if workspace == nil {
		return errors.New("workspace is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(command) == "" {
		return errors.New("check command must not be empty")
	}
	runner, err := workspace.runnerFor(workspace.root)
	if err != nil {
		return err
	}
	if timeout <= 0 {
		timeout = defaultShellTimeout
	}
	commandContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var output limitedBuffer
	output.limit = checkOutputBytes
	var inherited []*os.File
	if workspace.liveness != nil {
		token, err := workspace.liveness.DescendantLock()
		if err != nil {
			return fmt.Errorf("register check process liveness: %w", err)
		}
		defer token.Close()
		inherited = []*os.File{token}
	}
	err = runner.Run(commandContext, sandbox.Request{Command: command, WorkingDirectory: workspace.root, WorkspaceRoot: workspace.root, ReadOnly: true, InheritedFiles: inherited}, &output, &output)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(commandContext.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("check %q timed out after %s", command, timeout)
	}
	if exitError, ok := errors.AsType[*exec.ExitError](err); ok {
		return fmt.Errorf("check %q exited %d: %s", command, exitError.ExitCode(), strings.TrimSpace(output.String()))
	}
	return fmt.Errorf("run check %q: %w", command, err)
}

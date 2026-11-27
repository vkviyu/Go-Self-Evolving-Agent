package workspacecoord

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const maxGitOutputBytes = 32 << 20

// GitEnvironmentRunner is needed for snapshots: a normal git add must never
// change the worker's index. Test/custom runners must explicitly support this.
type GitEnvironmentRunner interface {
	RunWithEnvironment(context.Context, string, []string, ...string) ([]byte, error)
}

func (runner commandGit) RunWithEnvironment(ctx context.Context, directory string, environment []string, arguments ...string) ([]byte, error) {
	executable, err := runner.executable(directory)
	if err != nil {
		return nil, err
	}
	prefix := []string{"--no-pager", "-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false", "-c", "core.splitIndex=false"}
	command := exec.CommandContext(ctx, executable, append(prefix, arguments...)...)
	command.Dir = directory
	// Git metadata operations are not entitled to the model/API credentials or
	// arbitrary startup hooks carried by the host's environment.
	command.Env = []string{"PATH=" + filepath.Dir(executable) + string(os.PathListSeparator) + "/usr/bin" + string(os.PathListSeparator) + "/bin", "LANG=C", "LC_ALL=C"}
	command.Env = append(command.Env, environment...)
	command.Env = append(command.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1")
	stdout, stderr := &boundedGitOutput{limit: maxGitOutputBytes}, &boundedGitOutput{limit: 64 << 10}
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(arguments, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

type boundedGitOutput struct {
	buffer bytes.Buffer
	limit  int
}

// Bytes returns the retained portion of Git output.
func (output *boundedGitOutput) Bytes() []byte { return output.buffer.Bytes() }

// String returns the retained Git output as text.
func (output *boundedGitOutput) String() string { return output.buffer.String() }

func (output *boundedGitOutput) Write(data []byte) (int, error) {
	if len(data) > output.limit-output.buffer.Len() {
		return 0, fmt.Errorf("git output exceeds %d bytes; worktree must be retained", output.limit)
	}
	return output.buffer.Write(data)
}

func (manager *WorktreeManager) capturePatch(ctx context.Context, view View) ([]byte, error) {
	if git, ok := manager.Git.(commandGit); ok {
		return git.captureProtectedPatch(ctx, manager.Root, view)
	}
	git, ok := manager.Git.(GitEnvironmentRunner)
	if !ok {
		return nil, errors.New("git runner does not support isolated-index patch capture")
	}
	if strings.HasPrefix(view.BaseRevision, "-") {
		return nil, errors.New("invalid base revision")
	}
	temporary, err := os.MkdirTemp("", "gsea-worktree-index-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(temporary)
	environment := []string{"GIT_INDEX_FILE=" + filepath.Join(temporary, "index"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
	run := func(arguments ...string) ([]byte, error) {
		prefix := []string{"--literal-pathspecs", "-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false", "-c", "core.splitIndex=false"}
		return git.RunWithEnvironment(ctx, view.RootPath, environment, append(prefix, arguments...)...)
	}
	if _, err := run("read-tree", view.BaseRevision); err != nil {
		return nil, err
	}
	// Includes new files/deletions/binary content, but not ignored build output.
	if _, err := run("add", "--all", "--", "."); err != nil {
		return nil, err
	}
	return run("diff", "--cached", "--binary", "--full-index", "--no-renames", "--no-color", "--no-ext-diff", "--no-textconv", "--src-prefix=a/", "--dst-prefix=b/", view.BaseRevision, "--")
}

// PatchPaths lists the paths a captured patch writes, in patch order. It
// reads the "diff --git a/<path> b/<path>" headers capturePatch emits.
func PatchPaths(patch []byte) []string {
	var paths []string
	for line := range bytes.Lines(patch) {
		header, ok := bytes.CutPrefix(line, []byte("diff --git a/"))
		if !ok {
			continue
		}
		header = bytes.TrimRight(header, "\r\n")
		// Without renames both sides name the same path.
		if half := (len(header) - len(" b/")) / 2; half > 0 && string(header[half:half+3]) == " b/" {
			header = header[:half]
		}
		paths = append(paths, string(header))
	}
	return paths
}

// PatchDiff renders a captured patch for an approval or a review. Output
// beyond planDiffLimit is cut with a note.
func PatchDiff(patch []byte) string {
	if len(patch) <= planDiffLimit {
		return string(patch)
	}
	return string(patch[:planDiffLimit]) + fmt.Sprintf("\n... diff truncated at %d bytes\n", planDiffLimit)
}

func (manager *WorktreeManager) validateManagedPath(view View) error {
	if err := view.Validate(); err != nil {
		return err
	}
	root := manager.Worktrees
	if root == "" {
		root = filepath.Join(manager.Root, ".gsea", "worktrees")
	}
	relative, err := filepath.Rel(root, view.RootPath)
	if err != nil || relative == "." || !filepath.IsLocal(relative) {
		return errors.New("worktree path is not a child of the managed worktree directory")
	}
	return nil
}

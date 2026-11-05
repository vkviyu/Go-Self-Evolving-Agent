package evaluation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/vkviyu/go-self-evolving-agent/internal/sandbox"
)

const maximumPatchBytes = 32 << 20

type Worktree struct {
	Repository string
	Path       string
	BaseCommit string
	Mode       string
	root       string
	captureGit string // host-owned metadata, not the edited workspace .git
}

func PrepareWorktree(ctx context.Context, root string, task Task) (Worktree, error) {
	if normalizedWorkspaceMode(task) != WorkspaceGitWorktree {
		return Worktree{}, fmt.Errorf("PrepareWorktree requires workspace_mode %q", WorkspaceGitWorktree)
	}
	return prepareGitWorktree(ctx, root, task)
}

func PrepareWorkspace(ctx context.Context, root string, task Task) (Worktree, error) {
	switch normalizedWorkspaceMode(task) {
	case WorkspaceGitWorktree:
		return prepareGitWorktree(ctx, root, task)
	case WorkspaceDirectory:
		return prepareDirectoryCopy(ctx, root, task)
	default:
		return Worktree{}, fmt.Errorf("unsupported workspace_mode %q", task.WorkspaceMode)
	}
}

func prepareGitWorktree(ctx context.Context, root string, task Task) (Worktree, error) {
	repository, err := filepath.Abs(task.Repository)
	if err != nil {
		return Worktree{}, fmt.Errorf("resolve task repository: %w", err)
	}
	repository, err = filepath.EvalSymlinks(repository)
	if err != nil {
		return Worktree{}, fmt.Errorf("resolve task repository symlinks: %w", err)
	}
	info, err := os.Stat(repository)
	if err != nil || !info.IsDir() {
		return Worktree{}, fmt.Errorf("task repository is not a directory")
	}
	prospectiveWorkspaceRoot, err := canonicalProspectivePath(filepath.Join(root, "workspaces"))
	if err != nil {
		return Worktree{}, fmt.Errorf("resolve worktree root: %w", err)
	}
	if pathContains(repository, prospectiveWorkspaceRoot) {
		return Worktree{}, errors.New("git_worktree workspace root must not be inside the source repository")
	}
	base, err := runGit(ctx, repository, "rev-parse", "--verify", task.BaseCommit+"^{commit}")
	if err != nil {
		return Worktree{}, fmt.Errorf("resolve base commit: %w", err)
	}
	baseCommit := strings.TrimSpace(string(base))
	if baseCommit == "" {
		return Worktree{}, errors.New("git returned an empty base commit")
	}
	workspaceRoot := filepath.Join(root, "workspaces")
	if err := os.MkdirAll(workspaceRoot, 0o700); err != nil {
		return Worktree{}, fmt.Errorf("create worktree root: %w", err)
	}
	path := filepath.Join(workspaceRoot, safePathComponent(task.ID))
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return Worktree{}, fmt.Errorf("task worktree already exists: %s", path)
		}
		return Worktree{}, fmt.Errorf("inspect task worktree: %w", err)
	}
	if _, err := runGit(ctx, repository, "worktree", "add", "--detach", path, baseCommit); err != nil {
		return Worktree{}, fmt.Errorf("create task worktree: %w", err)
	}
	worktree := Worktree{
		Repository: repository, Path: path, BaseCommit: baseCommit,
		Mode: WorkspaceGitWorktree, root: workspaceRoot,
	}
	if err := worktree.prepareCapture(ctx); err != nil {
		_, cleanupErr := runGit(context.WithoutCancel(ctx), repository, "worktree", "remove", "--force", path)
		return Worktree{}, errors.Join(err, cleanupErr)
	}
	return worktree, nil
}

func canonicalProspectivePath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	missing := make([]string, 0, 4)
	probe := absolute
	for {
		_, statErr := os.Lstat(probe)
		if statErr == nil {
			canonical, evalErr := filepath.EvalSymlinks(probe)
			if evalErr != nil {
				return "", evalErr
			}
			for _, segment := range slices.Backward(missing) {
				canonical = filepath.Join(canonical, segment)
			}
			return canonical, nil
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return "", statErr
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", errors.New("path has no existing ancestor")
		}
		missing = append(missing, filepath.Base(probe))
		probe = parent
	}
}

func prepareDirectoryCopy(ctx context.Context, root string, task Task) (Worktree, error) {
	source, err := canonicalSourceDirectory(task.Repository)
	if err != nil {
		return Worktree{}, err
	}
	workspaceRoot := filepath.Join(root, "workspaces")
	prospectiveWorkspaceRoot, err := canonicalProspectivePath(workspaceRoot)
	if err != nil {
		return Worktree{}, fmt.Errorf("resolve workspace root: %w", err)
	}
	if pathContains(source, prospectiveWorkspaceRoot) {
		return Worktree{}, errors.New("directory_copy workspace root must not be inside the source fixture")
	}
	if err := os.MkdirAll(workspaceRoot, 0o700); err != nil {
		return Worktree{}, fmt.Errorf("create workspace root: %w", err)
	}
	workspaceRoot, err = filepath.EvalSymlinks(workspaceRoot)
	if err != nil {
		return Worktree{}, fmt.Errorf("resolve workspace root symlinks: %w", err)
	}
	path := filepath.Join(workspaceRoot, safePathComponent(task.ID))
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return Worktree{}, fmt.Errorf("task workspace already exists: %s", path)
		}
		return Worktree{}, fmt.Errorf("inspect task workspace: %w", err)
	}
	if err := copyFixtureDirectory(source, path); err != nil {
		_ = os.RemoveAll(path)
		return Worktree{}, fmt.Errorf("copy task fixture: %w", err)
	}
	cleanupOnError := func(cause error) (Worktree, error) {
		return Worktree{}, errors.Join(cause, os.RemoveAll(path))
	}
	if _, err := runGit(ctx, path, "init", "--quiet"); err != nil {
		return cleanupOnError(fmt.Errorf("initialize task workspace: %w", err))
	}
	if _, err := runGit(ctx, path, "add", "--force", "--all", "--", "."); err != nil {
		return cleanupOnError(fmt.Errorf("stage task fixture baseline: %w", err))
	}
	if _, err := runGit(ctx, path,
		"-c", "user.name=GSEA Evaluation", "-c", "user.email=evaluation@example.test",
		"commit", "--quiet", "--allow-empty", "-m", "GSEA evaluation fixture baseline",
	); err != nil {
		return cleanupOnError(fmt.Errorf("commit task fixture baseline: %w", err))
	}
	base, err := runGit(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		return cleanupOnError(fmt.Errorf("resolve task fixture baseline: %w", err))
	}
	worktree := Worktree{
		Repository: source, Path: path, BaseCommit: strings.TrimSpace(string(base)),
		Mode: WorkspaceDirectory, root: workspaceRoot,
	}
	if err := worktree.prepareCapture(ctx); err != nil {
		return cleanupOnError(err)
	}
	return worktree, nil
}

// Capture metadata is created before handing the workspace to the Agent. It
// is outside that view and contains no Agent-provided Git configuration.
func (worktree *Worktree) prepareCapture(ctx context.Context) error {
	capture, err := os.MkdirTemp(worktree.root, ".gsea-eval-git-")
	if err != nil {
		return err
	}
	gitDirectory := filepath.Join(capture, "repository.git")
	if _, err := runGit(ctx, worktree.Path, "clone", "--bare", "--no-hardlinks", "--dissociate", "--", worktree.Path, gitDirectory); err != nil {
		_ = os.RemoveAll(capture)
		return fmt.Errorf("prepare protected patch metadata: %w", err)
	}
	worktree.captureGit = gitDirectory
	return nil
}

func canonicalSourceDirectory(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve task fixture: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve task fixture symlinks: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return "", errors.New("task fixture is not a directory")
	}
	return canonical, nil

}

func copyFixtureDirectory(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relative == ".git" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(destination, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case info.Mode().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(target, data, info.Mode().Perm())
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		default:
			return fmt.Errorf("fixture contains unsupported file type at %s", relative)
		}
	})
}

func pathContains(parent, candidate string) bool {
	relative, err := filepath.Rel(parent, candidate)
	return err == nil && filepath.IsLocal(relative)
}

func (worktree Worktree) Patch(ctx context.Context) ([]byte, error) {
	if worktree.captureGit == "" || worktree.Path == "" || worktree.BaseCommit == "" {
		return nil, errors.New("patch capture requires host-prepared workspace metadata")
	}
	return worktree.safePatch(ctx)
}

func (worktree Worktree) Remove(ctx context.Context) error {
	if worktree.captureGit != "" {
		parent := filepath.Dir(worktree.captureGit)
		if filepath.Dir(parent) != worktree.root || !strings.HasPrefix(filepath.Base(parent), ".gsea-eval-git-") {
			return errors.New("refusing to remove unrecognized capture metadata")
		}
		if err := os.RemoveAll(parent); err != nil {
			return err
		}
	}
	if strings.TrimSpace(worktree.Path) == "" {
		return nil
	}
	if worktree.Mode == WorkspaceDirectory {
		if strings.TrimSpace(worktree.root) == "" || !pathContains(worktree.root, worktree.Path) || worktree.Path == worktree.root {
			return errors.New("refusing to remove directory_copy workspace outside its managed root")
		}
		if err := os.RemoveAll(worktree.Path); err != nil {
			return fmt.Errorf("remove task workspace: %w", err)
		}
		return nil
	}
	if strings.TrimSpace(worktree.Repository) == "" {
		return nil
	}
	_, err := runGit(ctx, worktree.Repository, "worktree", "remove", "--force", worktree.Path)
	if err != nil {
		return fmt.Errorf("remove task worktree: %w", err)
	}
	return nil
}

func PatchSHA256(patch []byte) string {
	if len(patch) == 0 {
		return ""
	}
	sum := sha256.Sum256(patch)
	return hex.EncodeToString(sum[:])
}

func safePathComponent(identifier string) string {
	var safe strings.Builder
	for _, value := range identifier {
		if unicode.IsLetter(value) || unicode.IsDigit(value) || value == '-' || value == '_' || value == '.' {
			safe.WriteRune(value)
		} else {
			safe.WriteByte('_')
		}
	}
	sum := sha256.Sum256([]byte(identifier))
	name := strings.Trim(safe.String(), ".")
	if name == "" {
		name = "task"
	}
	if len(name) > 80 {
		name = name[:80]
	}
	return name + "-" + hex.EncodeToString(sum[:4])
}

func runGit(ctx context.Context, directory string, arguments ...string) ([]byte, error) {
	return runGitLimited(ctx, 4<<20, directory, arguments...)
}

func runGitLimited(ctx context.Context, limit int, directory string, arguments ...string) ([]byte, error) {
	arguments = append([]string{"--no-pager", "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null"}, arguments...)
	command := exec.CommandContext(ctx, "git", arguments...)
	command.Dir = directory
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_ATTR_NOSYSTEM=1", "LC_ALL=C"}
	var output cappedBuffer
	output.limit = limit
	var stderr cappedBuffer
	stderr.limit = 64 << 10
	command.Stdout = &output
	command.Stderr = &stderr
	err := command.Run()
	if output.truncated && err == nil {
		err = fmt.Errorf("git output exceeded %d bytes", limit)
	}
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(arguments, " "), err, strings.TrimSpace(stderr.String()))
	}
	return bytes.Clone(output.Bytes()), nil
}

func (worktree Worktree) safePatch(ctx context.Context) ([]byte, error) {
	runner, err := sandbox.New(worktree.Path)
	if err != nil {
		return nil, err
	}
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return nil, err
	}
	base := quote(gitPath) + " --no-pager --git-dir=" + quote(worktree.captureGit) + " --work-tree=" + quote(worktree.Path) + " -c core.bare=false -c core.fsmonitor=false -c core.hooksPath=/dev/null"
	command := "export GIT_INDEX_FILE=\"$TMPDIR/gsea-capture-index\" GIT_OBJECT_DIRECTORY=\"$TMPDIR/objects\" GIT_ALTERNATE_OBJECT_DIRECTORIES=" + quote(filepath.Join(worktree.captureGit, "objects")) + " GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_TERMINAL_PROMPT=0 GIT_ATTR_NOSYSTEM=1; mkdir -p \"$GIT_OBJECT_DIRECTORY\" && " + base + " read-tree " + quote(worktree.BaseCommit) + " && " + base + " add --intent-to-add --all -- . && " + base + " diff --binary --no-ext-diff --no-textconv " + quote(worktree.BaseCommit) + " -- . ':(exclude).gsea/artifacts/**' ':(exclude).gsea/worktrees/**'"
	var output, stderr cappedBuffer
	output.limit, stderr.limit = maximumPatchBytes, 64<<10
	err = runner.Run(ctx, sandbox.Request{Command: command, WorkingDirectory: worktree.Path, WorkspaceRoot: worktree.Path, ReadOnly: true, ReadOnlyPaths: []string{worktree.captureGit}}, &output, &stderr)
	if err != nil {
		return nil, fmt.Errorf("capture patch in sandbox: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if output.truncated {
		return nil, errors.New("patch exceeds output size limit")
	}
	return bytes.Clone(output.Bytes()), nil
}

type cappedBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (buffer *cappedBuffer) Write(data []byte) (int, error) {
	original := len(data)
	remaining := buffer.limit - buffer.Len()
	if remaining <= 0 {
		buffer.truncated = buffer.truncated || original > 0
		return original, nil
	}
	if len(data) > remaining {
		_, _ = buffer.Buffer.Write(data[:remaining])
		buffer.truncated = true
		return original, nil
	}
	_, _ = buffer.Buffer.Write(data)
	return original, nil
}

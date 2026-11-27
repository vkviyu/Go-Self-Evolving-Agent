package workspacecoord

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

// GitRunner is intentionally tiny so worktree behavior can be tested without
// invoking a real repository or coupling the coordinator to os/exec details.
type GitRunner interface {
	// Run executes Git with the supplied working directory and arguments and returns its
	// output.
	Run(context.Context, string, ...string) ([]byte, error)
}

type commandGit struct{ workspace string }

// Run executes Git in the requested directory with the workspace runner's command
// settings.
func (runner commandGit) Run(ctx context.Context, directory string, arguments ...string) ([]byte, error) {
	return runner.RunWithEnvironment(ctx, directory, nil, arguments...)
}

type WorktreeManager struct {
	Root      string
	Git       GitRunner
	Worktrees string
}

// NewWorktreeManager prepares the manager for Git worktrees associated with the workspace.
func NewWorktreeManager(root string) (*WorktreeManager, error) {
	root, err := filepath.Abs(strings.TrimSpace(root))
	if err != nil {
		return nil, fmt.Errorf("resolve worktree root: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("inspect worktree root: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("worktree root is not a directory")
	}
	return &WorktreeManager{Root: root, Git: commandGit{workspace: root}, Worktrees: filepath.Join(root, ".gsea", "worktrees")}, nil
}

// Create materializes a detached worktree for a Git view. Shared views do not
// need filesystem work and return without invoking Git.
func (manager *WorktreeManager) Create(ctx context.Context, view View) error {
	if manager == nil || manager.Git == nil {
		return errors.New("worktree manager is unavailable")
	}
	if view.Mode != ViewGitWorktree {
		return nil
	}
	if err := view.Validate(); err != nil {
		return err
	}
	if view.RootPath == "" || view.BaseRevision == "" {
		return errors.New("git view requires root path and base revision")
	}
	if err := manager.validateManagedPath(view); err != nil {
		return err
	}
	if _, err := os.Lstat(view.RootPath); err == nil {
		return errors.New("worktree destination already exists; refusing to replace it")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(view.RootPath), 0o700); err != nil {
		return fmt.Errorf("create worktree parent: %w", err)
	}
	if _, err := manager.Git.Run(ctx, manager.Root, "worktree", "add", "--detach", view.RootPath, view.BaseRevision); err != nil {
		return err
	}
	return nil
}

// Remove only releases pristine views. Dirty/committed/ignored output remains
// on disk even after merge; reclaiming it requires a separate explicit GC step.
func (manager *WorktreeManager) Remove(ctx context.Context, view View) error {
	if manager == nil || manager.Git == nil {
		return errors.New("worktree manager is unavailable")
	}
	if view.Mode != ViewGitWorktree {
		return nil
	}
	if err := manager.validateManagedPath(view); err != nil {
		return err
	}
	patch, err := manager.Diff(ctx, view)
	if err != nil {
		return err
	}
	if len(patch) != 0 {
		return errors.New("worktree has output relative to its base; automatic removal refused")
	}
	status, err := manager.Git.Run(ctx, view.RootPath, "status", "--porcelain=v1", "--untracked-files=all", "--ignored")
	if err != nil {
		return err
	}
	if len(status) != 0 {
		return errors.New("worktree contains staged, untracked or ignored files; automatic removal refused")
	}
	head, err := manager.Git.Run(ctx, view.RootPath, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(head)) != view.BaseRevision {
		return errors.New("worktree HEAD differs from its base; automatic removal refused")
	}
	_, err = manager.Git.Run(ctx, manager.Root, "worktree", "remove", view.RootPath)
	return err
}

// Diff returns a binary-safe patch relative to the view base revision. The
// caller should store it as a content-addressed artifact before queueing merge.
func (manager *WorktreeManager) Diff(ctx context.Context, view View) ([]byte, error) {
	if manager == nil || manager.Git == nil {
		return nil, errors.New("worktree manager is unavailable")
	}
	if view.Mode != ViewGitWorktree {
		return nil, errors.New("diff requires a git worktree view")
	}
	if strings.TrimSpace(view.BaseRevision) == "" {
		return nil, errors.New("git view base revision is required")
	}
	if err := manager.validateManagedPath(view); err != nil {
		return nil, err
	}
	return manager.capturePatch(ctx, view)
}

// ApplyPatch applies a binary-safe patch to the main workspace. The temporary
// file is removed regardless of the Git outcome, while Git retains conflict
// markers in the workspace for a resolution task to inspect.
func (manager *WorktreeManager) ApplyPatch(ctx context.Context, patch []byte) error {
	if manager == nil || manager.Git == nil {
		return errors.New("worktree manager is unavailable")
	}
	temporary, err := os.CreateTemp(manager.Root, ".gsea-merge-*.patch")
	if err != nil {
		return fmt.Errorf("create merge patch: %w", err)
	}
	name := temporary.Name()
	defer os.Remove(name)
	if _, err := temporary.Write(patch); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write merge patch: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync merge patch: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close merge patch: %w", err)
	}
	_, err = manager.Git.Run(ctx, manager.Root, "apply", "--3way", "--whitespace=nowarn", name)
	if err == nil {
		return nil
	}
	// A crash after Git applied the patch but before the durable state update
	// leaves a running merge. Treat that exact already-applied state as success.
	if _, reverseErr := manager.Git.Run(ctx, manager.Root, "apply", "--reverse", "--check", name); reverseErr == nil {
		return nil
	}
	return err
}

// ResolveHead reads the baseline revision used when a view is created.
func (manager *WorktreeManager) ResolveHead(ctx context.Context) (string, error) {
	if manager == nil || manager.Git == nil {
		return "", errors.New("worktree manager is unavailable")
	}
	output, err := manager.Git.Run(ctx, manager.Root, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	revision := strings.TrimSpace(string(output))
	if revision == "" {
		return "", errors.New("git returned an empty HEAD revision")
	}
	return revision, nil
}

func DefaultWorktreePath(root string, viewID orchestration.WorkspaceViewID) string {
	return filepath.Join(root, ".gsea", "worktrees", string(viewID))
}

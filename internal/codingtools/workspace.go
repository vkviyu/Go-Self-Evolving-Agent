// Package codingtools contains the built-in tools exposed to the coding Agent.
// Every operation is scoped to one immutable workspace root and returns JSON
// suitable for feeding back into Eino.
package codingtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/lsp"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/sandbox"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

// operationLeaseWait bounds how long a write or shell call waits for a live
// holder of an overlapping lease, possibly in another runtime instance.
const operationLeaseWait = 10 * time.Second

type (
	// Workspace is a process-local, canonical workspace boundary.
	Workspace struct {
		root            string
		shellRunner     sandbox.Runner
		shellSandboxErr error
		leases          orchestration.LeaseRepository
		liveness        DescendantLiveness
		views           workspacecoord.Repository
		diagnostics     lsp.ChangeDiagnoser
		artifacts       DiffArtifactStore
		sandboxMode     sandbox.Mode
		// snapshotViews holds snapshot work trees outside the workspace root.
		snapshotViews string
		// viewRunners caches one shell runner per snapshot work tree.
		viewRunners map[string]sandbox.Runner
		runnerMutex sync.Mutex
	}

	// DescendantLiveness extends this runtime's lease liveness to shell
	// processes. A process that outlives the runtime keeps its leases until
	// it exits, so another instance never writes beside an orphaned command.
	DescendantLiveness interface {
		DescendantLock() (*os.File, error)
	}
)

// AttachLanguageDiagnostics enables passive semantic feedback after
// structured file changes and publishes the active workspace_diagnostics
// tool. The service is optional and never changes write success semantics.
func (workspace *Workspace) AttachLanguageDiagnostics(diagnostics lsp.ChangeDiagnoser) {
	if workspace != nil {
		workspace.diagnostics = diagnostics
	}
}

// AttachDiffArtifactStore enables durable retrieval for large unified diffs.
// Diff persistence is best effort and never changes a successful mutation.
func (workspace *Workspace) AttachDiffArtifactStore(store DiffArtifactStore) {
	if workspace != nil {
		workspace.artifacts = store
	}
}

// AttachLeaseRepository enables durable workspace coordination for write
// tools. It is optional so isolated unit tests and read-only consumers can
// keep using Workspace without a database.
func (workspace *Workspace) AttachLeaseRepository(repository orchestration.LeaseRepository) {
	if workspace != nil {
		workspace.leases = repository
	}
}

// AttachDescendantLiveness makes every shell command inherit a liveness
// descriptor from the coordination domain that grants this workspace's leases.
func (workspace *Workspace) AttachDescendantLiveness(liveness DescendantLiveness) {
	if workspace != nil {
		workspace.liveness = liveness
	}
}

// AttachWorkspaceViewRepository enables Agent-scoped workspace roots. When a
// view is not found, the canonical workspace root remains the default so the
// root conversation and standalone tool tests keep their existing behavior.
func (workspace *Workspace) AttachWorkspaceViewRepository(repository workspacecoord.Repository) {
	if workspace != nil {
		workspace.views = repository
	}
}

// AttachSnapshotViewsDirectory admits snapshot views below directory. Their
// work trees live outside the workspace root, so each gets its own shell
// sandbox rooted at the work tree.
func (workspace *Workspace) AttachSnapshotViewsDirectory(directory string) error {
	if workspace == nil {
		return errors.New("workspace is unavailable")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create snapshot views directory: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return fmt.Errorf("resolve snapshot views directory: %w", err)
	}
	if canonical, err = filepath.Abs(canonical); err != nil {
		return fmt.Errorf("resolve snapshot views directory: %w", err)
	}
	workspace.snapshotViews = canonical
	return nil
}

// insideSnapshotViews reports whether root is a snapshot work tree.
func (workspace *Workspace) insideSnapshotViews(root string) bool {
	return workspace.snapshotViews != "" && root != workspace.snapshotViews && ensureInsideRoot(workspace.snapshotViews, root) == nil
}

// passiveDiagnostics reports whether writes below root get automatic LSP
// feedback. Snapshot work trees skip it; on-demand diagnostics still run.
func (workspace *Workspace) passiveDiagnostics(root string) bool {
	return workspace.diagnostics != nil && !workspace.insideSnapshotViews(root)
}

// runnerFor returns the shell runner whose sandbox covers root.
func (workspace *Workspace) runnerFor(root string) (sandbox.Runner, error) {
	if !workspace.insideSnapshotViews(root) {
		if workspace.shellRunner == nil {
			if workspace.shellSandboxErr != nil {
				return nil, fmt.Errorf("shell execution refused: %w", workspace.shellSandboxErr)
			}
			return nil, errors.New("shell execution refused: sandbox runner is unavailable")
		}
		return workspace.shellRunner, nil
	}
	workspace.runnerMutex.Lock()
	defer workspace.runnerMutex.Unlock()
	if runner, ok := workspace.viewRunners[root]; ok {
		return runner, nil
	}
	runner, err := sandbox.NewWithMode(root, workspace.sandboxMode)
	if err != nil {
		return nil, fmt.Errorf("shell execution refused: %w", err)
	}
	if workspace.viewRunners == nil {
		workspace.viewRunners = make(map[string]sandbox.Runner)
	}
	// Forget runners of snapshots that were removed.
	for cached := range workspace.viewRunners {
		if _, err := os.Stat(cached); errors.Is(err, os.ErrNotExist) {
			delete(workspace.viewRunners, cached)
		}
	}
	workspace.viewRunners[root] = runner
	return runner, nil
}

func (workspace *Workspace) operationRoot(ctx context.Context) (string, workspacecoord.ViewMode, error) {
	if workspace == nil {
		return "", "", errors.New("workspace is unavailable")
	}
	root, mode := workspace.root, workspacecoord.ViewSharedLease
	if workspace.views == nil {
		return root, mode, nil
	}
	actor, ok := orchestration.ActorScopeFromContext(ctx)
	if !ok || actor.IsZero() {
		return root, mode, nil
	}
	views, err := workspace.views.ListViews(ctx, workspacecoord.ViewQuery{
		TeamID: actor.TeamID, AgentID: actor.AgentID, States: []workspacecoord.ViewState{workspacecoord.ViewActive},
	})
	if err != nil {
		return "", "", fmt.Errorf("resolve workspace view: %w", err)
	}
	if len(views) == 0 {
		return root, mode, nil
	}
	view := views[len(views)-1]
	viewRoot, err := filepath.Abs(view.RootPath)
	if err != nil {
		return "", "", fmt.Errorf("resolve workspace view root: %w", err)
	}
	viewRoot, err = filepath.EvalSymlinks(viewRoot)
	if err != nil {
		return "", "", fmt.Errorf("resolve workspace view symlinks: %w", err)
	}
	// Snapshot views and read-only reviews of them live outside the root.
	reviewsSnapshot := view.Mode == workspacecoord.ViewSharedReadonly && viewRoot != workspace.root
	if view.Mode == workspacecoord.ViewSnapshot || reviewsSnapshot {
		if !workspace.insideSnapshotViews(viewRoot) {
			return "", "", fmt.Errorf("snapshot view root %q is outside the snapshot views directory", viewRoot)
		}
	} else if err := ensureInsideRoot(workspace.root, viewRoot); err != nil {
		return "", "", fmt.Errorf("workspace view root is outside canonical workspace: %w", err)
	}
	if _, err := os.Stat(viewRoot); err != nil {
		return "", "", fmt.Errorf("inspect workspace view root: %w", err)
	}
	return viewRoot, view.Mode, nil
}

func (workspace *Workspace) ensureWritable(ctx context.Context) (string, workspacecoord.ViewMode, error) {
	root, mode, err := workspace.operationRoot(ctx)
	if err != nil {
		return "", "", err
	}
	if mode == workspacecoord.ViewSharedReadonly {
		return "", "", errors.New("workspace view is read-only")
	}
	return root, mode, nil
}

// withWriteLease runs a file mutation under a write lease on that file.
func (workspace *Workspace) withWriteLease(ctx context.Context, relative string, fn func() (json.RawMessage, error)) (json.RawMessage, error) {
	return workspace.withLease(ctx, relative, "workspace write", fn)
}

// withWorkspaceLease runs a shell command under a write lease on the whole
// workspace because the command may touch any path.
func (workspace *Workspace) withWorkspaceLease(ctx context.Context, fn func() (json.RawMessage, error)) (json.RawMessage, error) {
	return workspace.withLease(ctx, orchestration.WorkspaceRootLeasePath, "shell execution", fn)
}

// withLease holds a write lease on path while fn runs. Isolated views
// (git worktree, snapshot) and unleased workspaces run fn directly; operation
// names the mutation in the actor error and the release reason.
func (workspace *Workspace) withLease(ctx context.Context, path, operation string, fn func() (json.RawMessage, error)) (json.RawMessage, error) {
	_, mode, err := workspace.ensureWritable(ctx)
	if err != nil {
		return nil, err
	}
	if mode == workspacecoord.ViewGitWorktree || mode == workspacecoord.ViewSnapshot || workspace.leases == nil {
		return fn()
	}
	actor, ok := orchestration.ActorScopeFromContext(ctx)
	if !ok || actor.IsZero() {
		return nil, fmt.Errorf("%s requires an Agent actor scope", operation)
	}
	lease, err := workspace.leases.AcquireLease(ctx, orchestration.LeaseSpec{
		TeamID: actor.TeamID, AgentID: actor.AgentID, RunID: actor.RunID, Path: path,
		Mode: orchestration.LeaseWrite, Wait: operationLeaseWait, CorrelationID: orchestration.ToolCallIDFromContext(ctx), IdempotencyKey: leaseIdempotencyKey(ctx),
	})
	if err != nil {
		return nil, err
	}
	defer func() {
		_, _ = workspace.leases.ReleaseLease(context.WithoutCancel(ctx), lease.ID, actor.AgentID, operation+" completed")
	}()
	return fn()
}

func leaseIdempotencyKey(ctx context.Context) string {
	callID := orchestration.ToolCallIDFromContext(ctx)
	if callID == "" {
		// A direct library caller has no durable Tool Pipeline call to retry.
		// Reusing a constant key would return a released lease and leave the
		// next mutation unprotected, so omit idempotency in that mode.
		return ""
	}
	return "lease:" + callID
}

// New validates and canonicalizes a workspace root.
func New(root string) (*Workspace, error) {
	return NewWithSandboxMode(root, sandbox.ModeOS)
}

// NewWithSandboxMode creates a workspace whose shell tool uses the requested
// isolation boundary. Container mode is reserved for callers that already
// run the entire Agent inside a disposable container.
func NewWithSandboxMode(root string, mode sandbox.Mode) (*Workspace, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, errors.New("workspace directory is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace directory: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace symlinks: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return nil, fmt.Errorf("inspect workspace directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace path %q is not a directory", root)
	}
	runner, runnerErr := sandbox.NewWithMode(canonical, mode)
	return &Workspace{root: canonical, shellRunner: runner, shellSandboxErr: runnerErr, sandboxMode: mode}, nil
}

// Root returns the canonical workspace path.
func (workspace *Workspace) Root() string {
	if workspace == nil {
		return ""
	}
	return workspace.root
}

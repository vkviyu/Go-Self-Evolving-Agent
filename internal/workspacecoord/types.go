// Package workspacecoord owns durable workspace views, fingerprints and the
// merge queue. It does not execute model calls or render user interfaces.
package workspacecoord

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

type ViewMode string

const (
	ViewSharedLease    ViewMode = "shared-lease"
	ViewSharedReadonly ViewMode = "shared-readonly"
	ViewGitWorktree    ViewMode = "git-worktree"
	// ViewSnapshot is a private copy of the workspace outside the workspace
	// root. Its BaseRevision is the digest of the baseline manifest.
	ViewSnapshot ViewMode = "snapshot"
)

type ViewState string

const (
	ViewActive   ViewState = "active"
	ViewReleased ViewState = "released"
)

type ViewSpec struct {
	TeamID         orchestration.TeamID
	AgentID        orchestration.AgentID
	TaskID         orchestration.TaskID
	Mode           ViewMode
	WorkspaceRoot  string
	RootPath       string
	BaseRevision   string
	CorrelationID  string
	IdempotencyKey string
}

func (spec ViewSpec) Validate() error {
	if err := spec.TeamID.Validate(); err != nil {
		return fmt.Errorf("workspace view team: %w", err)
	}
	if err := spec.AgentID.Validate(); err != nil {
		return fmt.Errorf("workspace view Agent: %w", err)
	}
	if spec.TaskID != "" {
		if err := spec.TaskID.Validate(); err != nil {
			return fmt.Errorf("workspace view task: %w", err)
		}
	}
	switch spec.Mode {
	case ViewSharedLease, ViewSharedReadonly, ViewGitWorktree, ViewSnapshot:
	default:
		return fmt.Errorf("%w: unsupported workspace view mode %q", orchestration.ErrInvalidEvent, spec.Mode)
	}
	if strings.TrimSpace(spec.WorkspaceRoot) == "" {
		return errors.New("workspace view root is required")
	}
	root, err := filepath.Abs(spec.WorkspaceRoot)
	if err != nil {
		return fmt.Errorf("workspace view root: %w", err)
	}
	if !filepath.IsAbs(root) {
		return errors.New("workspace view root must be absolute")
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("workspace view root symlinks: %w", err)
	}
	if strings.TrimSpace(spec.RootPath) == "" {
		return errors.New("workspace view path is required")
	}
	viewPath, err := filepath.Abs(spec.RootPath)
	if err != nil {
		return fmt.Errorf("workspace view path: %w", err)
	}
	if spec.Mode == ViewSnapshot {
		return validateSnapshotView(spec, viewPath)
	}
	// A read-only view outside the root reviews a snapshot work tree.
	if spec.Mode == ViewSharedReadonly && viewPath != root {
		return validateSnapshotView(spec, viewPath)
	}
	if spec.Mode != ViewGitWorktree && viewPath != root {
		return fmt.Errorf("%w: shared workspace view must use workspace root", orchestration.ErrInvalidEvent)
	}
	ancestor := viewPath
	for {
		if _, statErr := os.Stat(ancestor); statErr == nil {
			canonicalAncestor, evalErr := filepath.EvalSymlinks(ancestor)
			if evalErr != nil {
				return fmt.Errorf("workspace view path symlinks: %w", evalErr)
			}
			relative, relErr := filepath.Rel(canonicalRoot, canonicalAncestor)
			if relErr != nil || !filepath.IsLocal(relative) {
				return fmt.Errorf("%w: workspace view path escapes workspace root", orchestration.ErrInvalidEvent)
			}
			break
		}
		next := filepath.Dir(ancestor)
		if next == ancestor {
			return errors.New("workspace view path has no existing ancestor")
		}
		ancestor = next
	}
	if spec.Mode == ViewGitWorktree {
		relative, relErr := filepath.Rel(root, viewPath)
		if relErr != nil || !filepath.IsLocal(relative) {
			return fmt.Errorf("%w: git worktree path must stay inside workspace root", orchestration.ErrInvalidEvent)
		}
	}
	if spec.Mode == ViewGitWorktree && strings.TrimSpace(spec.BaseRevision) == "" {
		return fmt.Errorf("%w: git worktree view requires a base revision", orchestration.ErrInvalidEvent)
	}
	return nil
}

// validateSnapshotView checks a view rooted at a snapshot work tree. The
// work tree lives in the runtime data directory, so it is not required to be
// inside the workspace root.
func validateSnapshotView(spec ViewSpec, viewPath string) error {
	if filepath.Base(viewPath) != snapshotWorkDirectory {
		return fmt.Errorf("%w: snapshot view path must name its work tree", orchestration.ErrInvalidEvent)
	}
	if !strings.HasPrefix(strings.TrimSpace(spec.BaseRevision), "sha256:") {
		return fmt.Errorf("%w: snapshot view requires a baseline manifest digest", orchestration.ErrInvalidEvent)
	}
	return nil
}

type View struct {
	ID             orchestration.WorkspaceViewID `json:"view_id"`
	TeamID         orchestration.TeamID          `json:"team_id"`
	AgentID        orchestration.AgentID         `json:"agent_id"`
	TaskID         orchestration.TaskID          `json:"task_id,omitempty"`
	Mode           ViewMode                      `json:"mode"`
	WorkspaceRoot  string                        `json:"workspace_root"`
	RootPath       string                        `json:"root_path"`
	BaseRevision   string                        `json:"base_revision,omitempty"`
	State          ViewState                     `json:"state"`
	CorrelationID  string                        `json:"correlation_id,omitempty"`
	IdempotencyKey string                        `json:"idempotency_key,omitempty"`
	CreatedAt      time.Time                     `json:"created_at"`
	UpdatedAt      time.Time                     `json:"updated_at"`
	ReleasedAt     *time.Time                    `json:"released_at,omitempty"`
}

func (view View) Validate() error {
	return (ViewSpec{
		TeamID: view.TeamID, AgentID: view.AgentID, TaskID: view.TaskID,
		Mode: view.Mode, WorkspaceRoot: view.WorkspaceRoot, RootPath: view.RootPath,
		BaseRevision: view.BaseRevision,
	}).Validate()
}

type ViewQuery struct {
	TeamID  orchestration.TeamID
	AgentID orchestration.AgentID
	States  []ViewState
}

type MergeState string

const (
	MergePending    MergeState = "pending"
	MergeRunning    MergeState = "running"
	MergeApplied    MergeState = "applied"
	MergeConflicted MergeState = "conflicted"
	MergeRejected   MergeState = "rejected"
	// MergeSuperseded marks a request replaced by a newer candidate, such as
	// the output of its conflict resolution. It is never requeued.
	MergeSuperseded MergeState = "superseded"
)

type MergeKind string

const (
	// MergeGitPatch applies a git patch captured from a worktree view.
	MergeGitPatch MergeKind = "git-patch"
	// MergeSnapshot applies a snapshot candidate through an apply plan.
	MergeSnapshot MergeKind = "snapshot"
)

type MergeSpec struct {
	TeamID        orchestration.TeamID
	AgentID       orchestration.AgentID
	TaskID        orchestration.TaskID
	ViewID        orchestration.WorkspaceViewID
	BaseRevision  string
	PatchArtifact string
	// Kind defaults to MergeGitPatch. Snapshot merges store the candidate
	// artifact in PatchArtifact and the baseline digest in BaseRevision.
	Kind            MergeKind
	CandidateDigest string
	// Supersedes names an open merge that this request replaces. The old
	// request is marked superseded in the same transaction.
	Supersedes     orchestration.MergeID
	CorrelationID  string
	IdempotencyKey string
}

func (spec MergeSpec) Validate() error {
	if err := spec.TeamID.Validate(); err != nil {
		return fmt.Errorf("merge team: %w", err)
	}
	if err := spec.AgentID.Validate(); err != nil {
		return fmt.Errorf("merge Agent: %w", err)
	}
	if err := spec.ViewID.Validate(); err != nil {
		return fmt.Errorf("merge view: %w", err)
	}
	if spec.TaskID != "" {
		if err := spec.TaskID.Validate(); err != nil {
			return fmt.Errorf("merge task: %w", err)
		}
	}
	if strings.TrimSpace(spec.BaseRevision) == "" || strings.TrimSpace(spec.PatchArtifact) == "" {
		return fmt.Errorf("%w: merge base revision and patch artifact are required", orchestration.ErrInvalidEvent)
	}
	switch spec.Kind {
	case "", MergeGitPatch:
	case MergeSnapshot:
		if strings.TrimSpace(spec.CandidateDigest) == "" {
			return fmt.Errorf("%w: snapshot merge requires a candidate digest", orchestration.ErrInvalidEvent)
		}
	default:
		return fmt.Errorf("%w: unsupported merge kind %q", orchestration.ErrInvalidEvent, spec.Kind)
	}
	if spec.Supersedes != "" {
		if err := spec.Supersedes.Validate(); err != nil {
			return fmt.Errorf("superseded merge: %w", err)
		}
	}
	return nil
}

type MergeRequest struct {
	ID               orchestration.MergeID         `json:"merge_id"`
	TeamID           orchestration.TeamID          `json:"team_id"`
	AgentID          orchestration.AgentID         `json:"agent_id"`
	TaskID           orchestration.TaskID          `json:"task_id,omitempty"`
	ViewID           orchestration.WorkspaceViewID `json:"view_id"`
	BaseRevision     string                        `json:"base_revision"`
	PatchArtifact    string                        `json:"patch_artifact"`
	ResolutionTaskID orchestration.TaskID          `json:"resolution_task_id,omitempty"`
	Kind             MergeKind                     `json:"kind"`
	CandidateDigest  string                        `json:"candidate_digest,omitempty"`
	PlanDigest       string                        `json:"plan_digest,omitempty"`
	Supersedes       orchestration.MergeID         `json:"supersedes,omitempty"`
	SupersededBy     orchestration.MergeID         `json:"superseded_by,omitempty"`
	State            MergeState                    `json:"state"`
	Reason           string                        `json:"reason,omitempty"`
	CorrelationID    string                        `json:"correlation_id,omitempty"`
	IdempotencyKey   string                        `json:"idempotency_key,omitempty"`
	Version          uint64                        `json:"version"`
	CreatedAt        time.Time                     `json:"created_at"`
	UpdatedAt        time.Time                     `json:"updated_at"`
}

type MergeQuery struct {
	TeamID           orchestration.TeamID
	States           []MergeState
	ViewID           orchestration.WorkspaceViewID
	TaskID           orchestration.TaskID
	ResolutionTaskID orchestration.TaskID
}

type MergeTransition struct {
	MergeID          orchestration.MergeID
	Expected         uint64
	Target           MergeState
	ResolutionTaskID orchestration.TaskID
	// PlanDigest records the apply plan a snapshot merge commits.
	PlanDigest    string
	Reason        string
	ActorAgentID  orchestration.AgentID
	CorrelationID string
}

type Repository interface {
	CreateView(context.Context, ViewSpec) (View, error)
	GetView(context.Context, orchestration.WorkspaceViewID) (View, error)
	ListViews(context.Context, ViewQuery) ([]View, error)
	ReleaseView(context.Context, orchestration.WorkspaceViewID, string) (View, error)
	EnqueueMerge(context.Context, MergeSpec) (MergeRequest, error)
	GetMerge(context.Context, orchestration.MergeID) (MergeRequest, error)
	ListMerges(context.Context, MergeQuery) ([]MergeRequest, error)
	TransitionMerge(context.Context, MergeTransition) (MergeRequest, error)
}

// ResolutionLinker is an optional repository extension used to persist the
// task created for a conflicted merge. Keeping it optional preserves the small
// core repository contract for in-memory adapters.
type ResolutionLinker interface {
	LinkMergeResolutionTask(context.Context, orchestration.MergeID, orchestration.TaskID) (MergeRequest, error)
}

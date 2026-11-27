package orchestration

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const WorkspaceRootLeasePath = "__workspace__"

const (
	LeaseRead  LeaseMode = "read"
	LeaseWrite LeaseMode = "write"
)

type (
	LeaseMode string

	LeaseSpec struct {
		TeamID  TeamID
		AgentID AgentID
		RunID   RunID
		Path    string
		Mode    LeaseMode
		// Wait bounds how long a request polls while live holders keep a
		// conflicting lease. Zero fails on the first conflict.
		Wait           time.Duration
		CorrelationID  string
		IdempotencyKey string
	}

	// WorkspaceLease is held by one runtime instance. It stays active while
	// that instance or a descendant process it started is alive; there is no
	// time-based expiry.
	WorkspaceLease struct {
		ID             LeaseID    `json:"lease_id"`
		InstanceID     string     `json:"instance_id"`
		TeamID         TeamID     `json:"team_id"`
		AgentID        AgentID    `json:"agent_id"`
		RunID          RunID      `json:"run_id,omitempty"`
		Path           string     `json:"path"`
		Mode           LeaseMode  `json:"mode"`
		State          LeaseState `json:"state"`
		Epoch          uint64     `json:"epoch"`
		CorrelationID  string     `json:"correlation_id,omitempty"`
		IdempotencyKey string     `json:"idempotency_key,omitempty"`
		ReleaseReason  string     `json:"release_reason,omitempty"`
		CreatedAt      time.Time  `json:"created_at"`
		UpdatedAt      time.Time  `json:"updated_at"`
		ReleasedAt     *time.Time `json:"released_at,omitempty"`
	}

	// LeaseQuery filters leases of one workspace. An empty TeamID selects
	// every Team, including Teams owned by other runtime instances.
	LeaseQuery struct {
		TeamID  TeamID
		AgentID AgentID
		States  []LeaseState
	}

	LeaseRepository interface {
		// AcquireLease claims the specified workspace lease subject to ownership and
		// conflict checks.
		AcquireLease(context.Context, LeaseSpec) (WorkspaceLease, error)
		// ReleaseLease records the requesting agent's release of a lease and the supplied
		// reason.
		ReleaseLease(context.Context, LeaseID, AgentID, string) (WorkspaceLease, error)
		// ListLeases returns workspace leases matching the query filters.
		ListLeases(context.Context, LeaseQuery) ([]WorkspaceLease, error)
		// ReapAbandonedLeases reclaims leases whose holder instance and all of
		// its descendant processes have exited.
		ReapAbandonedLeases(context.Context) (int, error)
	}
)

func (spec LeaseSpec) Validate() error {
	if err := spec.TeamID.Validate(); err != nil {
		return fmt.Errorf("lease team: %w", err)
	}
	if err := spec.AgentID.Validate(); err != nil {
		return fmt.Errorf("lease Agent: %w", err)
	}
	if spec.RunID != "" {
		if err := spec.RunID.Validate(); err != nil {
			return fmt.Errorf("lease run: %w", err)
		}
	}
	path := strings.TrimSpace(spec.Path)
	if !validLeasePath(path) {
		return fmt.Errorf("%w: lease path must be workspace-relative", ErrInvalidEvent)
	}
	switch spec.Mode {
	case LeaseRead, LeaseWrite:
	default:
		return fmt.Errorf("%w: unsupported lease mode %q", ErrInvalidEvent, spec.Mode)
	}
	if spec.Wait < 0 {
		return fmt.Errorf("%w: lease wait must not be negative", ErrInvalidEvent)
	}
	return nil
}

func validLeasePath(path string) bool {
	if path == "" || path == WorkspaceRootLeasePath || strings.IndexByte(path, 0) >= 0 || filepath.IsAbs(path) {
		return path == WorkspaceRootLeasePath
	}
	// Without ".." components, cleaning cannot climb above the workspace, so
	// only a path naming the workspace itself remains to reject.
	normalized := strings.ReplaceAll(path, `\`, "/")
	if slices.Contains(strings.Split(normalized, "/"), "..") {
		return false
	}
	return filepath.ToSlash(filepath.Clean(filepath.FromSlash(normalized))) != "."
}

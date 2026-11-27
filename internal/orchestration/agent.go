package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

var (
	ErrAgentLimit         = errors.New("Agent limit reached")
	ErrAgentStateConflict = errors.New("Agent state version conflict")
)

type AgentSnapshot struct {
	ID              AgentID          `json:"agent_id"`
	TeamID          TeamID           `json:"team_id"`
	ParentAgentID   AgentID          `json:"parent_agent_id"`
	ThreadID        ThreadID         `json:"thread_id"`
	SessionID       session.ID       `json:"session_id"`
	AssignedTaskID  TaskID           `json:"task_id,omitempty"`
	InitialPrompt   string           `json:"-"`
	ContextPolicy   string           `json:"context_policy"`
	ProfileRevision string           `json:"profile_revision"`
	ProfileSnapshot json.RawMessage  `json:"profile_snapshot,omitempty"`
	Workspace       WorkspaceRequest `json:"workspace,omitempty"`
	WorkspaceMode   WorkspaceMode    `json:"workspace_mode,omitempty"`
	CurrentRunID    RunID            `json:"current_run_id,omitempty"`
	State           AgentState       `json:"state"`
	Activity        string           `json:"activity,omitempty"`
	Depth           int              `json:"depth"`
	Version         uint64           `json:"version"`
	CreatedAt       time.Time        `json:"created_at"`
	UpdatedAt       time.Time        `json:"updated_at"`
	Result          *AgentResult     `json:"result,omitempty"`
}

// ActorScope returns the identity that must accompany work executed by this
// Agent. It intentionally excludes the parent relationship from mutation.
func (agent AgentSnapshot) ActorScope() ActorScope {
	return ActorScope{
		TeamID:          agent.TeamID,
		AgentID:         agent.ID,
		ThreadID:        agent.ThreadID,
		TaskID:          agent.AssignedTaskID,
		RunID:           agent.CurrentRunID,
		ParentAgentID:   agent.ParentAgentID,
		ProfileRevision: agent.ProfileRevision,
	}
}

type ResultStatus string

const (
	ResultSucceeded ResultStatus = "succeeded"
	ResultPartial   ResultStatus = "partial"
	ResultBlocked   ResultStatus = "blocked"
	ResultFailed    ResultStatus = "failed"
	ResultCanceled  ResultStatus = "canceled"
)

type AgentResult struct {
	Status   ResultStatus    `json:"status"`
	Summary  string          `json:"summary"`
	Payload  json.RawMessage `json:"payload,omitempty"`
	Blockers []string        `json:"blockers,omitempty"`
	Risks    []string        `json:"risks,omitempty"`
	Error    string          `json:"error,omitempty"`
	// Retryable is set by a trusted runner when it has classified a failure as
	// safe to assign to a new Agent attempt. It is never inferred from text.
	Retryable      bool                   `json:"retryable,omitempty"`
	RetryCode      TaskAttemptFailureCode `json:"retry_code,omitempty"`
	OutcomeUnknown bool                   `json:"outcome_unknown,omitempty"`
	// Delivery names the snapshot candidate the run queued for merge. It is
	// set by the Supervisor, never by the child.
	Delivery *WorkspaceDelivery `json:"delivery,omitempty"`
}

// WorkspaceDelivery identifies a frozen snapshot candidate and the merge
// request that carries it to the main workspace.
type WorkspaceDelivery struct {
	ViewID          WorkspaceViewID `json:"view_id"`
	TaskID          TaskID          `json:"task_id,omitempty"`
	ExecutionID     string          `json:"execution_id,omitempty"`
	ContractHash    string          `json:"contract_hash,omitempty"`
	Attempt         int             `json:"attempt,omitempty"`
	CandidateDigest string          `json:"candidate_digest"`
	MergeID         MergeID         `json:"merge_id"`
	ChangedPaths    int             `json:"changed_paths"`
}

// Validate checks that the result has a recognized status and any attached payload is
// valid JSON.
func (result AgentResult) Validate() error {
	switch result.Status {
	case ResultSucceeded, ResultPartial, ResultBlocked, ResultFailed, ResultCanceled:
	default:
		return errors.New("Agent result has an unsupported status")
	}
	if len(result.Payload) > 0 && !json.Valid(result.Payload) {
		return errors.New("Agent result payload must be valid JSON")
	}
	return nil
}

type SpawnSpec struct {
	TeamID          TeamID
	ParentAgentID   AgentID
	ProfileName     string
	ProfileRevision string
	ProfileSnapshot json.RawMessage
	TaskID          TaskID
	// TaskVersion binds a generated prompt to the exact plan it describes.
	// Zero keeps compatibility with callers that do not dispatch a saved plan.
	TaskVersion   uint64
	InitialPrompt string
	ContextPolicy string
	// Workspace is the requested workspace access. It is part of the
	// durable identity, so a retry must request the same access.
	Workspace WorkspaceRequest
	// WorkspaceMode is the access the Supervisor granted for Workspace. Like
	// ProfileSnapshot it is set by trusted code, never by tool arguments.
	WorkspaceMode  WorkspaceMode
	IdempotencyKey string
	CorrelationID  string
}

// WorkspaceRequest is the workspace access a spawner asks for. The Supervisor
// resolves it once at spawn time; recovery reuses the persisted result rather
// than resolving the request again under different defaults.
type WorkspaceRequest string

// WorkspaceMode is the workspace access granted to a child Agent; values
// match the workspace view modes. Agents created before the mode was
// persisted have an empty mode and derive it from their profile.
type WorkspaceMode string

const (
	// WorkspaceAuto keeps the workspace mode of the child's profile.
	WorkspaceAuto WorkspaceRequest = "auto"
	// WorkspaceReadonly narrows the child to read-only workspace access.
	WorkspaceReadonly WorkspaceRequest = "readonly"

	WorkspaceModeSharedReadonly WorkspaceMode = "shared-readonly"
	WorkspaceModeSharedLease    WorkspaceMode = "shared-lease"
	WorkspaceModeGitWorktree    WorkspaceMode = "git-worktree"
	// WorkspaceModeSnapshot gives the child a private copy of the workspace
	// whose changes reach main only through the merge queue.
	WorkspaceModeSnapshot WorkspaceMode = "snapshot"
)

// Normalize maps an omitted request to auto and rejects modes that are not
// implemented, so an unknown value never falls back to writable access.
func (request WorkspaceRequest) Normalize() (WorkspaceRequest, error) {
	switch request {
	case "", WorkspaceAuto:
		return WorkspaceAuto, nil
	case WorkspaceReadonly:
		return request, nil
	default:
		return "", fmt.Errorf("%w: unsupported workspace access %q", ErrInvalidEvent, request)
	}
}

// Validate accepts every granted mode and the empty legacy value.
func (mode WorkspaceMode) Validate() error {
	switch mode {
	case "", WorkspaceModeSharedReadonly, WorkspaceModeSharedLease, WorkspaceModeGitWorktree, WorkspaceModeSnapshot:
		return nil
	default:
		return fmt.Errorf("%w: unsupported workspace mode %q", ErrInvalidEvent, mode)
	}
}

type AgentQuery struct {
	TeamID        TeamID
	ParentAgentID AgentID
	States        []AgentState
}

type AgentTransition struct {
	AgentID       AgentID
	Expected      uint64
	Target        AgentState
	Activity      string
	RunID         RunID
	CausationID   string
	CorrelationID string
	Reason        string
	Result        *AgentResult
}

type AgentRepository interface {
	// CreateChild persists the child under its parent, using the spawn identity to
	// recognize duplicate requests.
	CreateChild(ctx context.Context, spec SpawnSpec) (AgentSnapshot, error)
	// FindSpawn looks up a previous spawn by its team-scoped idempotency key.
	FindSpawn(ctx context.Context, teamID TeamID, idempotencyKey string) (AgentSnapshot, bool, error)
	// ListTeams returns the team IDs known to the agent repository.
	ListTeams(ctx context.Context) ([]TeamID, error)
	// GetAgent loads the agent's current snapshot by ID, including its initial prompt.
	GetAgent(ctx context.Context, agentID AgentID) (AgentSnapshot, error)
	// ListAgents returns the agent snapshots matching the supplied query.
	ListAgents(ctx context.Context, query AgentQuery) ([]AgentSnapshot, error)
	// TransitionAgent applies an allowed state change against the expected agent version
	// and returns the updated snapshot.
	TransitionAgent(ctx context.Context, transition AgentTransition) (AgentSnapshot, error)
}

// SpawnRequestFinder is implemented by repositories that can validate an
// idempotent spawn request before admission limits are evaluated. The
// Supervisor uses it as an optimization; CreateChild remains the durable
// transaction boundary and must repeat the check for races.
type SpawnRequestFinder interface {
	// FindSpawnRequest looks up a prior spawn and checks whether its recorded identity
	// matches the supplied spec.
	FindSpawnRequest(ctx context.Context, spec SpawnSpec) (AgentSnapshot, bool, error)
}

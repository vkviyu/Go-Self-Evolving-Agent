package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// WaitSetState is the durable lifecycle of an Agent suspension.
type WaitSetState string

const (
	WaitSetPending  WaitSetState = "pending"
	WaitSetResolved WaitSetState = "resolved"
	WaitSetExpired  WaitSetState = "expired"
	WaitSetCanceled WaitSetState = "canceled"
	WaitSetConsumed WaitSetState = "consumed"
)

// WaitSpec describes a suspension without carrying process-local channels or
// contexts. Target agents are evaluated by the Supervisor after a durable
// state change, so a restart can resume the same condition.
type WaitSpec struct {
	TeamID         TeamID
	AgentID        AgentID
	RunID          RunID
	ToolCallID     string
	Condition      string
	TargetAgentIDs []AgentID
	Any            bool
	Deadline       *time.Time
	ResumeToken    string
	CorrelationID  string
	IdempotencyKey string
}

// Validate checks the wait owner, condition and targets, rejecting duplicate targets and
// self-waits.
func (spec WaitSpec) Validate() error {
	if err := spec.TeamID.Validate(); err != nil {
		return fmt.Errorf("wait team: %w", err)
	}
	if err := spec.AgentID.Validate(); err != nil {
		return fmt.Errorf("wait Agent: %w", err)
	}
	if spec.RunID != "" {
		if err := spec.RunID.Validate(); err != nil {
			return fmt.Errorf("wait run: %w", err)
		}
	}
	if spec.Condition == "" {
		return fmt.Errorf("%w: wait condition is required", ErrInvalidEvent)
	}
	if len(spec.TargetAgentIDs) == 0 {
		return fmt.Errorf("%w: wait requires at least one target Agent", ErrInvalidEvent)
	}
	seen := make(map[AgentID]struct{}, len(spec.TargetAgentIDs))
	for _, target := range spec.TargetAgentIDs {
		if err := target.Validate(); err != nil {
			return fmt.Errorf("wait target: %w", err)
		}
		if target == spec.AgentID {
			return fmt.Errorf("%w: Agent cannot wait for itself", ErrInvalidEvent)
		}
		if _, exists := seen[target]; exists {
			return fmt.Errorf("%w: wait target %s is duplicated", ErrInvalidEvent, target)
		}
		seen[target] = struct{}{}
	}
	if spec.Deadline != nil && spec.Deadline.IsZero() {
		return fmt.Errorf("%w: wait deadline must be non-zero", ErrInvalidEvent)
	}
	return nil
}

type WaitSet struct {
	ID             WaitSetID    `json:"wait_set_id"`
	TeamID         TeamID       `json:"team_id"`
	AgentID        AgentID      `json:"agent_id"`
	RunID          RunID        `json:"run_id,omitempty"`
	ToolCallID     string       `json:"tool_call_id,omitempty"`
	Condition      string       `json:"condition"`
	TargetAgentIDs []AgentID    `json:"target_agent_ids"`
	Any            bool         `json:"any"`
	Deadline       *time.Time   `json:"deadline,omitempty"`
	ResumeToken    string       `json:"resume_token,omitempty"`
	State          WaitSetState `json:"state"`
	CorrelationID  string       `json:"correlation_id,omitempty"`
	IdempotencyKey string       `json:"idempotency_key,omitempty"`
	CreatedAt      time.Time    `json:"created_at"`
	UpdatedAt      time.Time    `json:"updated_at"`
	ResolvedAt     *time.Time   `json:"resolved_at,omitempty"`
}

// WaitSuspendedError signals that a runner reached a durable wait boundary.
// It is intentionally defined in the domain package so adapters can recognize
// it without importing the Supervisor package.
type WaitSuspendedError struct {
	WaitSet WaitSet
}

// WaitSpec reconstructs the immutable wait intent carried by a suspension.
// Its ID may be empty: a runner checkpoints before Supervisor commits the
// WaitSet and releases the execution slot.
func (err *WaitSuspendedError) WaitSpec() WaitSpec {
	wait := err.WaitSet
	return WaitSpec{TeamID: wait.TeamID, AgentID: wait.AgentID, RunID: wait.RunID,
		ToolCallID: wait.ToolCallID, Condition: wait.Condition, TargetAgentIDs: append([]AgentID(nil), wait.TargetAgentIDs...),
		Any: wait.Any, Deadline: wait.Deadline, ResumeToken: wait.ResumeToken,
		CorrelationID: wait.CorrelationID, IdempotencyKey: wait.IdempotencyKey}
}

const WaitInterruptPrefix = "gsea.agent_wait.v1:"

// EncodeWaitInterrupt uses a plain string so framework checkpoints do not
// depend on gob registrations or Go package names for domain values.
func EncodeWaitInterrupt(wait WaitSet) (string, error) {
	if err := (&WaitSuspendedError{WaitSet: wait}).WaitSpec().Validate(); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(wait)
	return WaitInterruptPrefix + string(encoded), err
}

// DecodeWaitInterrupt distinguishes this host control flow from other Eino
// interruptions. Framework-specific adapters own how this string is carried.
func DecodeWaitInterrupt(encoded string) (*WaitSuspendedError, bool, error) {
	if len(encoded) < len(WaitInterruptPrefix) || encoded[:len(WaitInterruptPrefix)] != WaitInterruptPrefix {
		return nil, false, nil
	}
	var wait WaitSet
	if err := json.Unmarshal([]byte(encoded[len(WaitInterruptPrefix):]), &wait); err != nil {
		return nil, true, fmt.Errorf("decode Agent wait suspension: %w", err)
	}
	result := &WaitSuspendedError{WaitSet: wait}
	if err := result.WaitSpec().Validate(); err != nil {
		return nil, true, err
	}
	if wait.ToolCallID == "" || wait.ResumeToken == "" || wait.IdempotencyKey == "" {
		return nil, true, errors.New("Agent wait suspension lacks its durable call identity")
	}
	return result, true, nil
}

type waitIntentKey struct{}

// WithWaitIntent carries a previously recorded wait across framework replay.
// It preserves the original absolute deadline rather than extending timeouts.
func WithWaitIntent(ctx context.Context, wait WaitSet) context.Context {
	return context.WithValue(ctx, waitIntentKey{}, wait)
}

// WaitIntentFromContext retrieves the durable wait intent attached for a replayed tool
// call.
func WaitIntentFromContext(ctx context.Context) (WaitSet, bool) {
	wait, ok := ctx.Value(waitIntentKey{}).(WaitSet)
	return wait, ok
}

func (err *WaitSuspendedError) Error() string {
	if err == nil {
		return "Agent wait suspended"
	}
	return fmt.Sprintf("Agent suspended on wait set %s", err.WaitSet.ID)
}

type WaitSetQuery struct {
	TeamID  TeamID
	AgentID AgentID
	States  []WaitSetState
}

type WaitRepository interface {
	// CreateWaitSet persists the conditions that would wake the agent after suspension.
	CreateWaitSet(context.Context, WaitSpec) (WaitSet, error)
	// GetWaitSet reads the wait intent and its current lifecycle state by ID.
	GetWaitSet(context.Context, WaitSetID) (WaitSet, error)
	// ListWaitSets returns persisted waits matching the query filters.
	ListWaitSets(context.Context, WaitSetQuery) ([]WaitSet, error)
	// ResolveWaitSet records that the wait condition has been satisfied.
	ResolveWaitSet(context.Context, WaitSetID, string) (WaitSet, error)
	// ConsumeWaitSet marks a resolved or expired wait as consumed by its continuation.
	ConsumeWaitSet(context.Context, WaitSetID, string) (WaitSet, error)
	// ExpireWaitSet records that the wait has reached its deadline.
	ExpireWaitSet(context.Context, WaitSetID, string) (WaitSet, error)
	// CancelWaitSetsForAgent cancels outstanding waits owned by the agent with the
	// supplied reason.
	CancelWaitSetsForAgent(context.Context, AgentID, string) error
}

// WaitSetTargetsJSON is shared by durable implementations and tests that need
// to serialize target IDs without exposing a database-specific representation.
func WaitSetTargetsJSON(targets []AgentID) ([]byte, error) {
	return json.Marshal(targets)
}

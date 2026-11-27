// Package orchestration defines durable Multi-Agent control-plane contracts.
package orchestration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

type TeamID string
type AgentID string
type ThreadID string
type TaskID string
type RunID string
type MessageID string
type ApprovalID string
type LeaseID string
type EventID string
type WorkspaceViewID string
type WaitSetID string
type MergeID string

// NewTeamID returns a new UUIDv7 in the team_ namespace.
func NewTeamID() (TeamID, error) { return newTypedID[TeamID]("team") }

// NewAgentID returns a new UUIDv7 in the agent_ namespace.
func NewAgentID() (AgentID, error) { return newTypedID[AgentID]("agent") }

// NewThreadID returns a new UUIDv7 in the thread_ namespace.
func NewThreadID() (ThreadID, error) { return newTypedID[ThreadID]("thread") }

// NewTaskID returns a new UUIDv7 in the task_ namespace.
func NewTaskID() (TaskID, error) { return newTypedID[TaskID]("task") }

// NewRunID returns a new UUIDv7 in the run_ namespace.
func NewRunID() (RunID, error) { return newTypedID[RunID]("run") }

// NewMessageID returns a new UUIDv7 in the msg_ namespace.
func NewMessageID() (MessageID, error) { return newTypedID[MessageID]("msg") }

// NewApprovalID returns a new UUIDv7 in the approval_ namespace.
func NewApprovalID() (ApprovalID, error) { return newTypedID[ApprovalID]("approval") }

// NewLeaseID returns a new UUIDv7 in the lease_ namespace.
func NewLeaseID() (LeaseID, error) { return newTypedID[LeaseID]("lease") }

// NewEventID returns a new UUIDv7 in the event_ namespace.
func NewEventID() (EventID, error) { return newTypedID[EventID]("event") }

// NewWorkspaceViewID returns a new UUIDv7 in the view_ namespace.
func NewWorkspaceViewID() (WorkspaceViewID, error) { return newTypedID[WorkspaceViewID]("view") }

// NewWaitSetID returns a new UUIDv7 in the wait_ namespace.
func NewWaitSetID() (WaitSetID, error) { return newTypedID[WaitSetID]("wait") }

// NewMergeID returns a new UUIDv7 in the merge_ namespace.
func NewMergeID() (MergeID, error) { return newTypedID[MergeID]("merge") }

// Validate checks the team_ prefix and the UUIDv7 payload.
func (id TeamID) Validate() error { return validateTypedID(string(id), "team") }

// Validate checks the agent_ prefix and the UUIDv7 payload.
func (id AgentID) Validate() error { return validateTypedID(string(id), "agent") }

// Validate checks the thread_ prefix and the UUIDv7 payload.
func (id ThreadID) Validate() error { return validateTypedID(string(id), "thread") }

// Validate checks the task_ prefix and the UUIDv7 payload.
func (id TaskID) Validate() error { return validateTypedID(string(id), "task") }

// Validate checks the run_ prefix and the UUIDv7 payload.
func (id RunID) Validate() error { return validateTypedID(string(id), "run") }

// Validate checks the msg_ prefix and the UUIDv7 payload.
func (id MessageID) Validate() error { return validateTypedID(string(id), "msg") }

// Validate checks the approval_ prefix and the UUIDv7 payload.
func (id ApprovalID) Validate() error { return validateTypedID(string(id), "approval") }

// Validate checks the lease_ prefix and the UUIDv7 payload.
func (id LeaseID) Validate() error { return validateTypedID(string(id), "lease") }

// Validate checks the event_ prefix and the UUIDv7 payload.
func (id EventID) Validate() error { return validateTypedID(string(id), "event") }

// Validate checks the view_ prefix and the UUIDv7 payload.
func (id WorkspaceViewID) Validate() error { return validateTypedID(string(id), "view") }

// Validate checks the wait_ prefix and the UUIDv7 payload.
func (id WaitSetID) Validate() error { return validateTypedID(string(id), "wait") }

// Validate checks the merge_ prefix and the UUIDv7 payload.
func (id MergeID) Validate() error { return validateTypedID(string(id), "merge") }

type typedID interface {
	~string
}

// newTypedID constructs a prefixed UUIDv7 using the current Unix millisecond timestamp and
// random bits.
func newTypedID[T typedID](prefix string) (T, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate %s ID: %w", prefix, err)
	}
	milliseconds := uint64(time.Now().UTC().UnixMilli())
	raw[0] = byte(milliseconds >> 40)
	raw[1] = byte(milliseconds >> 32)
	raw[2] = byte(milliseconds >> 24)
	raw[3] = byte(milliseconds >> 16)
	raw[4] = byte(milliseconds >> 8)
	raw[5] = byte(milliseconds)
	raw[6] = raw[6]&0x0f | 0x70
	raw[8] = raw[8]&0x3f | 0x80
	return T(prefix + "_" + hex.EncodeToString(raw[:])), nil
}

// validateTypedID checks the expected prefix, hexadecimal payload, UUID version and
// variant.
func validateTypedID(value, prefix string) error {
	wantPrefix := prefix + "_"
	if !strings.HasPrefix(value, wantPrefix) {
		return fmt.Errorf("%s ID must start with %q", prefix, wantPrefix)
	}
	encoded := strings.TrimPrefix(value, wantPrefix)
	if len(encoded) != 32 {
		return fmt.Errorf("%s ID payload must contain 32 hexadecimal characters", prefix)
	}
	raw, err := hex.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("decode %s ID: %w", prefix, err)
	}
	if len(raw) != 16 || raw[6]>>4 != 7 || raw[8]>>6 != 2 {
		return fmt.Errorf("%s ID is not a UUIDv7 value", prefix)
	}
	return nil
}

type ActorScope struct {
	TeamID          TeamID
	AgentID         AgentID
	ThreadID        ThreadID
	TaskID          TaskID
	RunID           RunID
	ParentAgentID   AgentID
	ProfileRevision string
}

type actorScopeContextKey struct{}
type toolCallIDContextKey struct{}
type turnIDContextKey struct{}

// WithActorScope attaches durable actor identity to a tool execution context.
func WithActorScope(ctx context.Context, scope ActorScope) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, actorScopeContextKey{}, scope)
}

// ActorScopeFromContext returns the actor identity attached by the runtime.
func ActorScopeFromContext(ctx context.Context) (ActorScope, bool) {
	if ctx == nil {
		return ActorScope{}, false
	}
	scope, ok := ctx.Value(actorScopeContextKey{}).(ActorScope)
	return scope, ok
}

// WithToolCallID attaches the provider's stable tool-call identity to a tool
// execution context. Control tools use it as their default idempotency key.
func WithToolCallID(ctx context.Context, callID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, toolCallIDContextKey{}, callID)
}

// ToolCallIDFromContext returns the attached provider tool-call identity.
func ToolCallIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(toolCallIDContextKey{}).(string)
	return value
}

// WithTurnID attaches the durable runtime turn identity to a tool context.
// Control tools use it as the checkpoint resume token when a turn suspends.
func WithTurnID(ctx context.Context, turnID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, turnIDContextKey{}, turnID)
}

// TurnIDFromContext returns the turn identity attached by the runtime.
func TurnIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(turnIDContextKey{}).(string)
	return value
}

// IsZero reports whether all actor identifiers and the profile revision are empty.
func (scope ActorScope) IsZero() bool {
	return scope.TeamID == "" && scope.AgentID == "" && scope.ThreadID == "" && scope.TaskID == "" &&
		scope.RunID == "" && scope.ParentAgentID == "" && scope.ProfileRevision == ""
}

// Validate requires team, agent and thread IDs and checks the optional identifiers when
// present.
func (scope ActorScope) Validate() error {
	if scope.TeamID == "" || scope.AgentID == "" || scope.ThreadID == "" {
		return errors.New("actor scope requires team, agent and thread IDs")
	}
	if err := scope.TeamID.Validate(); err != nil {
		return err
	}
	if err := scope.AgentID.Validate(); err != nil {
		return err
	}
	if err := scope.ThreadID.Validate(); err != nil {
		return err
	}
	if scope.TaskID != "" {
		if err := scope.TaskID.Validate(); err != nil {
			return err
		}
	}
	if scope.RunID != "" {
		if err := scope.RunID.Validate(); err != nil {
			return err
		}
	}
	if scope.ParentAgentID != "" {
		if err := scope.ParentAgentID.Validate(); err != nil {
			return err
		}
	}
	return nil
}

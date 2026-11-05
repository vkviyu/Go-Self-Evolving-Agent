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

func NewTeamID() (TeamID, error)                   { return newTypedID[TeamID]("team") }
func NewAgentID() (AgentID, error)                 { return newTypedID[AgentID]("agent") }
func NewThreadID() (ThreadID, error)               { return newTypedID[ThreadID]("thread") }
func NewTaskID() (TaskID, error)                   { return newTypedID[TaskID]("task") }
func NewRunID() (RunID, error)                     { return newTypedID[RunID]("run") }
func NewMessageID() (MessageID, error)             { return newTypedID[MessageID]("msg") }
func NewApprovalID() (ApprovalID, error)           { return newTypedID[ApprovalID]("approval") }
func NewLeaseID() (LeaseID, error)                 { return newTypedID[LeaseID]("lease") }
func NewEventID() (EventID, error)                 { return newTypedID[EventID]("event") }
func NewWorkspaceViewID() (WorkspaceViewID, error) { return newTypedID[WorkspaceViewID]("view") }
func NewWaitSetID() (WaitSetID, error)             { return newTypedID[WaitSetID]("wait") }
func NewMergeID() (MergeID, error)                 { return newTypedID[MergeID]("merge") }

func (id TeamID) Validate() error          { return validateTypedID(string(id), "team") }
func (id AgentID) Validate() error         { return validateTypedID(string(id), "agent") }
func (id ThreadID) Validate() error        { return validateTypedID(string(id), "thread") }
func (id TaskID) Validate() error          { return validateTypedID(string(id), "task") }
func (id RunID) Validate() error           { return validateTypedID(string(id), "run") }
func (id MessageID) Validate() error       { return validateTypedID(string(id), "msg") }
func (id ApprovalID) Validate() error      { return validateTypedID(string(id), "approval") }
func (id LeaseID) Validate() error         { return validateTypedID(string(id), "lease") }
func (id EventID) Validate() error         { return validateTypedID(string(id), "event") }
func (id WorkspaceViewID) Validate() error { return validateTypedID(string(id), "view") }
func (id WaitSetID) Validate() error       { return validateTypedID(string(id), "wait") }
func (id MergeID) Validate() error         { return validateTypedID(string(id), "merge") }

type typedID interface {
	~string
}

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

func (scope ActorScope) IsZero() bool {
	return scope.TeamID == "" && scope.AgentID == "" && scope.ThreadID == "" && scope.TaskID == "" &&
		scope.RunID == "" && scope.ParentAgentID == "" && scope.ProfileRevision == ""
}

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

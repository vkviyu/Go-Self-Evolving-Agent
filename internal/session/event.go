package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ID identifies a durable agent session.
type ID string

// EventType names a durable fact in a session timeline.
type EventType string

const (
	EventSessionCreated EventType = "session.created"
	EventTurnStarted    EventType = "turn.started"
	// EventTurnForegroundSelected records a user's choice to resume a foreground turn.
	EventTurnForegroundSelected EventType = "turn.foreground.selected"
	EventTurnPrepared           EventType = "turn.prepared"
	EventModelPartial           EventType = "model.partial"
	EventUserMessage            EventType = "user.message"
	// EventBackgroundInput records Agent messages added to a turn as untrusted
	// user-role context, either as the opening input of a wake turn or at a
	// position inside a running turn's model input.
	EventBackgroundInput    EventType = "background.input"
	EventModelRequested     EventType = "model.requested"
	EventModelFailed        EventType = "model.failed"
	EventAssistantMessage   EventType = "assistant.message"
	EventToolProposed       EventType = "tool.proposed"
	EventPolicyDecided      EventType = "policy.decided"
	EventApprovalRequested  EventType = "approval.requested"
	EventApprovalResolved   EventType = "approval.resolved"
	EventToolStarted        EventType = "tool.started"
	EventToolSuspended      EventType = "tool.suspended"
	EventToolSucceeded      EventType = "tool.succeeded"
	EventToolFailed         EventType = "tool.failed"
	EventToolOutcomeUnknown EventType = "tool.outcome_unknown"
	EventCheckpointCreated  EventType = "checkpoint.created"
	EventContextCompacted   EventType = "context.compacted"
	EventContextManaged     EventType = "context.managed"
	EventTurnCompleted      EventType = "turn.completed"
	EventTurnFailed         EventType = "turn.failed"
	EventTurnAbandoned      EventType = "turn.abandoned"
	EventTurnArchived       EventType = "turn.archived"
)

const (
	// AnySequence disables optimistic sequence checking for a single append.
	// It is intended for the process-local serialized writer only.
	AnySequence uint64 = ^uint64(0)
)

var (
	// ErrSequenceConflict indicates that another writer advanced the session.
	ErrSequenceConflict = errors.New("session sequence conflict")
	// ErrInvalidEvent indicates an event that cannot be persisted.
	ErrInvalidEvent = errors.New("invalid session event")
)

// PendingEvent is an event before the store assigns identity, sequence and time.
type PendingEvent struct {
	Type              EventType
	Payload           json.RawMessage
	CausationID       string
	CorrelationID     string
	RuntimeSnapshotID string
}

// Validate checks the protocol-level invariants of a pending event.
func (e PendingEvent) Validate() error {
	if e.Type == "" {
		return fmt.Errorf("%w: event type is required", ErrInvalidEvent)
	}
	if len(e.Payload) == 0 || !json.Valid(e.Payload) {
		return fmt.Errorf("%w: payload must be valid JSON", ErrInvalidEvent)
	}
	return nil
}

// Event is an immutable fact persisted in a session timeline.
type Event struct {
	SessionID         ID
	Sequence          uint64
	EventID           string
	Type              EventType
	OccurredAt        time.Time
	CausationID       string
	CorrelationID     string
	RuntimeSnapshotID string
	Payload           json.RawMessage
	Checksum          string
	SchemaVersion     uint32
}

// EventStore is the durable append-only session event boundary.
//
// Checkpoint state intentionally lives behind CheckpointStore. Keeping the
// interfaces separate prevents event projections and tool execution from
// depending on framework-specific recovery storage.
type EventStore interface {
	// Append appends the batch to the session at the expected sequence. AnySequence
	// accepts the current head.
	Append(ctx context.Context, sessionID ID, expectedSequence uint64, events ...PendingEvent) ([]Event, error)
	// Load reads events in increasing sequence order, starting strictly after
	// afterSequence.
	Load(ctx context.Context, sessionID ID, afterSequence uint64, limit int) ([]Event, error)
	// CurrentSequence reports the current event sequence for the session.
	CurrentSequence(ctx context.Context, sessionID ID) (uint64, error)
}

// CorrelatedEventStore is an optional indexed read path for stores that can
// resolve one durable operation without scanning an entire session timeline.
// EventStore remains the compatibility boundary; correctness does not depend on
// this optimization.
type CorrelatedEventStore interface {
	// LoadByCorrelation retrieves this session's events belonging to the supplied
	// correlation ID.
	LoadByCorrelation(ctx context.Context, sessionID ID, correlationID string) ([]Event, error)
}

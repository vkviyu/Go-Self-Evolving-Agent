package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type StreamType string

const (
	StreamTeam      StreamType = "team"
	StreamAgent     StreamType = "agent"
	StreamThread    StreamType = "thread"
	StreamTask      StreamType = "task"
	StreamMailbox   StreamType = "mailbox"
	StreamWorkspace StreamType = "workspace"
	StreamApproval  StreamType = "approval"
	StreamBudget    StreamType = "budget"
	StreamFlow      StreamType = "flow"
)

type EventType string

const (
	EventTeamCreated         EventType = "team.created"
	EventTeamDraining        EventType = "team.draining_started"
	EventTeamCancelRequested EventType = "team.cancel_requested"
	EventTeamCompleted       EventType = "team.completed"
	EventTeamFailed          EventType = "team.failed"
	EventTeamCanceled        EventType = "team.canceled"
	// EventTeamReopened records a finished Team becoming active again.
	EventTeamReopened EventType = "team.reopened"

	EventAgentCreated         EventType = "agent.created"
	EventAgentQueued          EventType = "agent.queued"
	EventAgentRunAdmitted     EventType = "agent.run_admitted"
	EventAgentRunStarted      EventType = "agent.run_started"
	EventAgentRunFailed       EventType = "agent.run_failed"
	EventAgentRunCompleted    EventType = "agent.run_completed"
	EventAgentResultRecorded  EventType = "agent.result_recorded"
	EventAgentWaiting         EventType = "agent.wait_started"
	EventAgentWaitResolved    EventType = "agent.wait_resolved"
	EventWaitSetCreated       EventType = "wait_set.created"
	EventWaitSetResolved      EventType = "wait_set.resolved"
	EventWaitSetConsumed      EventType = "wait_set.consumed"
	EventWaitSetExpired       EventType = "wait_set.expired"
	EventWaitSetCanceled      EventType = "wait_set.canceled"
	EventAgentCancelRequested EventType = "agent.cancel_requested"
	EventAgentCompleted       EventType = "agent.completed"
	EventAgentFailed          EventType = "agent.failed"
	EventAgentCanceled        EventType = "agent.canceled"

	EventThreadCreated       EventType = "thread.created"
	EventThreadContextSeeded EventType = "thread.context_seeded"

	EventTaskCreated           EventType = "task.created"
	EventTaskUpdated           EventType = "task.updated"
	EventTaskDependencyAdded   EventType = "task.dependency_added"
	EventTaskDependencyRemoved EventType = "task.dependency_removed"
	EventTaskClaimExpired      EventType = "task.claim_expired"
	EventTaskReassigned        EventType = "task.reassigned"

	EventMailboxEnqueued     EventType = "mailbox.message_enqueued"
	EventMailboxDelivered    EventType = "mailbox.message_delivered"
	EventMailboxAcknowledged EventType = "mailbox.message_acknowledged"
	EventMailboxBound        EventType = "mailbox.message_bound"
	EventMailboxReleased     EventType = "mailbox.message_released"

	EventApprovalRequested EventType = "approval.requested"
	EventApprovalResolved  EventType = "approval.resolved"
	EventApprovalExpired   EventType = "approval.expired"
	EventApprovalCanceled  EventType = "approval.canceled"

	EventLeaseRequested         EventType = "workspace.lease_requested"
	EventLeaseActive            EventType = "workspace.lease_active"
	EventLeaseReleased          EventType = "workspace.lease_released"
	EventLeaseExpired           EventType = "workspace.lease_expired"
	EventLeaseConflicted        EventType = "workspace.lease_conflicted"
	EventWorkspaceViewCreated   EventType = "workspace.view_created"
	EventWorkspaceViewReleased  EventType = "workspace.view_released"
	EventWorkspaceViewPreserved EventType = "workspace.view_preserved"
	EventMergeQueued            EventType = "workspace.merge_queued"
	EventMergeStarted           EventType = "workspace.merge_started"
	EventMergeRequeued          EventType = "workspace.merge_requeued"
	EventMergeResolutionLinked  EventType = "workspace.merge_resolution_linked"
	EventMergeApplied           EventType = "workspace.merge_applied"
	EventMergeConflicted        EventType = "workspace.merge_conflicted"
	EventMergeRejected          EventType = "workspace.merge_rejected"
	EventMergeSuperseded        EventType = "workspace.merge_superseded"

	EventUsageRecorded   EventType = "usage.recorded"
	EventBudgetWarning   EventType = "budget.warning_reached"
	EventBudgetExhausted EventType = "budget.exhausted"

	EventFlowStarted       EventType = "flow.started"
	EventFlowStepStarted   EventType = "flow.step_started"
	EventFlowStepCompleted EventType = "flow.step_completed"
	EventFlowStepFailed    EventType = "flow.step_failed"
	EventFlowCompleted     EventType = "flow.completed"
	EventFlowFailed        EventType = "flow.failed"
	EventFlowCanceled      EventType = "flow.canceled"
)

const AnySequence uint64 = ^uint64(0)

var (
	ErrSequenceConflict    = errors.New("orchestration stream sequence conflict")
	ErrInvalidEvent        = errors.New("invalid orchestration event")
	ErrNotFound            = errors.New("orchestration entity not found")
	ErrIdempotencyConflict = errors.New("idempotency key was already used with different input")
	ErrLeaseConflict       = errors.New("workspace lease conflict")
)

type Stream struct {
	Type   StreamType
	ID     string
	TeamID TeamID
}

func (stream Stream) Validate() error {
	if stream.Type == "" || stream.ID == "" {
		return fmt.Errorf("%w: stream type and ID are required", ErrInvalidEvent)
	}
	if stream.TeamID == "" {
		return fmt.Errorf("%w: team ID is required", ErrInvalidEvent)
	}
	if err := stream.TeamID.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidEvent, err)
	}
	return nil
}

type PendingEvent struct {
	Type           EventType
	ActorAgentID   AgentID
	CausationID    string
	CorrelationID  string
	IdempotencyKey string
	Payload        json.RawMessage
}

func (event PendingEvent) Validate() error {
	if event.Type == "" {
		return fmt.Errorf("%w: event type is required", ErrInvalidEvent)
	}
	if len(event.Payload) == 0 || !json.Valid(event.Payload) {
		return fmt.Errorf("%w: payload must be valid JSON", ErrInvalidEvent)
	}
	if event.ActorAgentID != "" {
		if err := event.ActorAgentID.Validate(); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidEvent, err)
		}
	}
	return nil
}

type Event struct {
	EventID        EventID
	GlobalSequence uint64
	StreamType     StreamType
	StreamID       string
	StreamSequence uint64
	TeamID         TeamID
	Type           EventType
	OccurredAt     time.Time
	ActorAgentID   AgentID
	CausationID    string
	CorrelationID  string
	IdempotencyKey string
	Payload        json.RawMessage
	Checksum       string
	SchemaVersion  uint32
}

type Cursor uint64

type EventStore interface {
	Append(ctx context.Context, stream Stream, expectedSequence uint64, events ...PendingEvent) ([]Event, error)
	LoadStream(ctx context.Context, streamType StreamType, streamID string, afterSequence uint64, limit int) ([]Event, error)
	LoadGlobal(ctx context.Context, after Cursor, limit int) ([]Event, error)
	CurrentSequence(ctx context.Context, streamType StreamType, streamID string) (uint64, error)
}

func JSONPayload(value any) (json.RawMessage, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode orchestration payload: %w", err)
	}
	return payload, nil
}

func MustJSONPayload(value any) json.RawMessage {
	payload, err := JSONPayload(value)
	if err != nil {
		panic(err)
	}
	return payload
}

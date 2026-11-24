package agentruntime

import (
	"context"

	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

// EventType identifies a transient runtime notification. Transient events are
// intentionally separate from session.EventStore: they are for live consumers and
// are never used as conversation history.
type EventType string

const (
	EventMemoryReferenceLookupStarted        EventType = "memory.reference.lookup.started"
	EventTurnStarting                        EventType = "turn.starting"
	EventTurnResuming                        EventType = "turn.resuming"
	EventConversationLockWaiting             EventType = "conversation.lock.waiting"
	EventConversationHistoryLoading          EventType = "conversation.history.loading"
	EventTurnBuilding                        EventType = "turn.building"
	EventTurnStrategySelectionStarted        EventType = "turn.strategy.selection.started"
	EventTurnBackgroundReconciliationStarted EventType = "turn.background.reconciliation.started"
	EventTurnSnapshotRetentionStarted        EventType = "turn.snapshot.retention.started"
	EventTurnSessionSequenceReadStarted      EventType = "turn.session.sequence.read.started"
	EventTurnSessionCreationStarted          EventType = "turn.session.creation.started"
	EventTurnAgentConstructionStarted        EventType = "turn.agent.construction.started"
	EventTurnPersistenceStarted              EventType = "turn.persistence.started"
	EventAgentLoopStarting                   EventType = "agent.loop.starting"
	EventModelPreparationStarted             EventType = "model.preparation.started"
	EventExperiencePreparationStarted        EventType = "experience.preparation.started"
	EventContextTokenCountingStarted         EventType = "context.token.counting.started"
	EventFileReferenceInspectionStarted      EventType = "file.reference.inspection.started"
	EventTurnPreparedPersistenceStarted      EventType = "turn.prepared.persistence.started"
	EventExperienceSynchronizationStarted    EventType = "experience.synchronization.started"
	EventContextCompactionStarted            EventType = "context.compaction.started"
	EventModelBudgetReservationStarted       EventType = "model.budget.reservation.started"
	EventModelRequestPersistenceStarted      EventType = "model.request.persistence.started"
	EventModelDispatchPersistenceStarted     EventType = "model.dispatch.persistence.started"
	EventModelRequestStarted                 EventType = "model.request.started"
	EventModelResponseStarted                EventType = "model.response.started"
	EventModelToolFormatRetry                EventType = "model.tool_format.retry"
	EventModelTimeoutRetry                   EventType = "model.timeout.retry"
	EventToolProposed                        EventType = "tool.proposed"
	EventToolPolicyChecking                  EventType = "tool.policy.checking"
	EventToolPolicyDecided                   EventType = "tool.policy.decided"
	EventToolApprovalRequested               EventType = "tool.approval.requested"
	EventToolApprovalWaiting                 EventType = "tool.approval.waiting"
	EventToolApprovalResolved                EventType = "tool.approval.resolved"
	EventToolExecutionStarted                EventType = "tool.execution.started"
	EventToolExecutionSucceeded              EventType = "tool.execution.succeeded"
	EventToolExecutionFailed                 EventType = "tool.execution.failed"
	EventToolOutcomeUnknown                  EventType = "tool.outcome.unknown"
	EventTurnFinalizing                      EventType = "turn.finalizing"
	EventAssistantTextDelta                  EventType = "assistant.text.delta"
	EventAssistantReasoningDelta             EventType = "assistant.reasoning.delta"
	EventAssistantMessage                    EventType = "assistant.message.completed"
	EventToolResult                          EventType = "tool.result"
	EventRuntimeMessage                      EventType = "runtime.message"
)

// Event is the normalized live output of one Agent turn. Message is populated
// for a completed model or tool message. Delta is populated for streamed text
// or reasoning summary content.
type Event struct {
	Type     EventType
	TurnID   string
	Purpose  string
	CallID   string
	ToolName string
	Approved bool
	Error    string
	Delta    string
	Message  *schema.Message
}

// EventSink consumes live turn events. A nil sink disables live notifications
// without changing durable behavior.
type EventSink func(context.Context, Event) error

func emitEvent(ctx context.Context, sink EventSink, event Event) error {
	if sink == nil {
		return nil
	}
	return sink(ctx, event)
}

type eventSinkKey struct{}

// WithLiveSink binds both model and tool live notifications to one consumer.
// These notifications are transient and do not become session history.
func WithLiveSink(ctx context.Context, sink EventSink) context.Context {
	if sink == nil {
		return ctx
	}
	ctx = context.WithValue(ctx, eventSinkKey{}, sink)
	return tool.WithLiveEventSink(ctx, func(eventContext context.Context, event tool.LiveEvent) error {
		return emitEvent(eventContext, sink, toolLiveEvent(event))
	})
}

func EventSinkFromContext(ctx context.Context) EventSink {
	if ctx == nil {
		return nil
	}
	sink, _ := ctx.Value(eventSinkKey{}).(EventSink)
	return sink
}

func toolLiveEvent(event tool.LiveEvent) Event {
	eventType := EventRuntimeMessage
	switch event.Type {
	case tool.LiveToolProposed:
		eventType = EventToolProposed
	case tool.LivePolicyChecking:
		eventType = EventToolPolicyChecking
	case tool.LivePolicyDecided:
		eventType = EventToolPolicyDecided
	case tool.LiveApprovalRequested:
		eventType = EventToolApprovalRequested
	case tool.LiveApprovalWaiting:
		eventType = EventToolApprovalWaiting
	case tool.LiveApprovalResolved:
		eventType = EventToolApprovalResolved
	case tool.LiveToolStarted:
		eventType = EventToolExecutionStarted
	case tool.LiveToolSucceeded:
		eventType = EventToolExecutionSucceeded
	case tool.LiveToolFailed:
		eventType = EventToolExecutionFailed
	case tool.LiveToolOutcomeUnknown:
		eventType = EventToolOutcomeUnknown
	}
	result := Event{
		Type: eventType, TurnID: event.TurnID, CallID: event.CallID,
		ToolName: event.ToolName, Approved: event.Approved,
	}
	if event.Err != nil {
		result.Error = event.Err.Error()
	}
	return result
}

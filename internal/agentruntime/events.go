package agentruntime

import (
	"context"

	"github.com/cloudwego/eino/schema"
)

// EventType identifies a transient runtime notification. Transient events are
// intentionally separate from session.EventStore: they are for live consumers and
// are never used as conversation history.
type EventType string

const (
	EventAssistantTextDelta      EventType = "assistant.text.delta"
	EventAssistantReasoningDelta EventType = "assistant.reasoning.delta"
	EventAssistantMessage        EventType = "assistant.message.completed"
	EventToolResult              EventType = "tool.result"
	EventRuntimeMessage          EventType = "runtime.message"
	EventGoalPhase               EventType = "goal.phase"
)

// Event is the normalized live output of one Agent turn. Message is populated
// for a completed model or tool message. Delta is populated for streamed text
// or reasoning summary content.
type Event struct {
	Type      EventType
	TurnID    string
	Delta     string
	Message   *schema.Message
	GoalPhase *GoalPhase
}

// GoalPhase is presentation-only; durable goal state remains authoritative.
type GoalPhase struct {
	Stage     string // executing, verifying, retrying, stopped
	GoalID    string
	Round     int
	MaxRounds int
	State     string
	Reason    string
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

package conversation

import (
	"context"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
)

type candidateEventBuffer struct {
	downstream agentruntime.EventSink
	events     []agentruntime.Event
}

func (buffer *candidateEventBuffer) Sink(ctx context.Context, event agentruntime.Event) error {
	if event.Type == agentruntime.EventAssistantTextDelta || (event.Type == agentruntime.EventAssistantMessage && event.Message != nil && len(event.Message.ToolCalls) == 0) {
		buffer.events = append(buffer.events, event)
		return nil
	}
	// A model can emit a single assistant message containing both a draft
	// explanation and tool calls. Tool calls are visible immediately, while
	// candidate text must stay buffered until Goal Mode accepts the round.
	// Keep the original event immutable and suppress only that unaccepted text.
	if event.Type == agentruntime.EventAssistantMessage && event.Message != nil && len(event.Message.ToolCalls) > 0 && event.Message.Content != "" {
		message := *event.Message
		message.Content = ""
		event.Message = &message
	}
	if buffer.downstream == nil {
		return nil
	}
	return buffer.downstream(ctx, event)
}

func (buffer *candidateEventBuffer) Flush(ctx context.Context) error {
	if buffer.downstream == nil {
		return nil
	}
	for _, event := range buffer.events {
		if err := buffer.downstream(ctx, event); err != nil {
			return err
		}
	}
	return nil
}

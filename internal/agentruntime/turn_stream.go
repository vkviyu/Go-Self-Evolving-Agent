package agentruntime

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

// consumeIterator translates Eino's event variants into the runtime's small
// live-event vocabulary and returns the final assistant message.
func (t *Turn) consumeIterator(ctx context.Context, iterator *adk.AsyncIterator[*adk.AgentEvent], sink EventSink, resumed bool) (Result, error) {
	var final *schema.Message
	streamedAny := false
	for {
		event, ok := iterator.Next()
		if !ok {
			break
		}
		if event.Err != nil {
			return Result{}, t.fail(ctx, event.Err)
		}
		if event.Action != nil && event.Action.Interrupted != nil {
			for _, interrupt := range event.Action.Interrupted.InterruptContexts {
				info, ok := interrupt.Info.(string)
				if !ok {
					continue
				}
				suspended, matched, decodeErr := orchestration.DecodeWaitInterrupt(info)
				if decodeErr != nil {
					return Result{}, t.fail(ctx, decodeErr)
				}
				if matched {
					if err := t.recordJournalCheckpoint(ctx); err != nil {
						return Result{}, t.fail(ctx, err)
					}
					return Result{}, suspended
				}
			}
			return Result{}, t.fail(ctx, errors.New("Agent execution interrupted without a supported recovery boundary"))
		}
		if event.Output == nil || event.Output.MessageOutput == nil {
			continue
		}
		message, streamed, err := consumeMessageVariant(ctx, event.Output.MessageOutput, t.turnID, sink)
		if err != nil {
			return Result{}, t.fail(ctx, err)
		}
		streamedAny = streamedAny || streamed
		if message != nil && message.Role == schema.Assistant {
			final = message
		}
	}
	if final == nil || final.Content == "" || len(final.ToolCalls) > 0 {
		return Result{}, t.fail(ctx, errors.New("Eino Agent completed without an assistant message"))
	}
	if err := tool.CheckTurnCompletion(ctx, t.sessionID, t.turnID); err != nil {
		return Result{}, t.fail(ctx, err)
	}
	assessment := t.recordedModel.completedContext(final)
	payload := map[string]any{"turn_id": t.turnID, "content": final.Content, "resumed": resumed}
	if assessment != nil {
		payload["context_assessment"] = assessment
	}
	if _, err := t.events.Append(ctx, t.sessionID, session.AnySequence, session.PendingEvent{
		Type: session.EventTurnCompleted, CorrelationID: t.turnID, RuntimeSnapshotID: t.runtimeSnapshotID,
		Payload: session.MustJSONPayload(payload),
	}); err != nil {
		return Result{}, t.fail(ctx, fmt.Errorf("record Agent turn completion: %w", err))
	}
	// Completion is already durable. Cleanup failure must not turn successful
	// work into a retryable failed turn; mailbox settlement retries on open.
	cleanupContext, cancelCleanup := durabilityContext(ctx)
	defer cancelCleanup()
	if t.checkpoints != nil {
		_ = t.checkpoints.Delete(cleanupContext, t.checkpointID)
	}
	if t.mailbox != nil {
		// The turn has committed. A failed acknowledgement leaves the messages
		// bound to it, and the next settlement acknowledges them.
		_ = t.mailbox.Acknowledge(cleanupContext, t.turnID)
	}
	return Result{TurnID: t.turnID, RuntimeSnapshotID: t.runtimeSnapshotID, Final: final, Streamed: streamedAny, ContextUsage: assessment}, nil
}

func consumeMessageVariant(ctx context.Context, variant *adk.MessageVariant, turnID string, sink EventSink) (*schema.Message, bool, error) {
	if variant == nil {
		return nil, false, nil
	}
	if !variant.IsStreaming {
		return consumeCompleteMessage(ctx, variant, turnID, sink)
	}
	return consumeStreamingMessage(ctx, variant, turnID, sink)
}

func consumeCompleteMessage(ctx context.Context, variant *adk.MessageVariant, turnID string, sink EventSink) (*schema.Message, bool, error) {
	if variant.Message == nil {
		return nil, false, nil
	}
	role := variant.Role
	if role == "" {
		role = variant.Message.Role
	}
	typeOfEvent := EventRuntimeMessage
	switch role {
	case schema.Assistant:
		typeOfEvent = EventAssistantMessage
	case schema.Tool:
		typeOfEvent = EventToolResult
	}
	eventMessage, err := cloneMessage(variant.Message)
	if err != nil {
		return nil, false, err
	}
	if role == schema.Assistant && variant.Message.ReasoningContent != "" {
		if err := emitEvent(ctx, sink, Event{Type: EventAssistantReasoningDelta, TurnID: turnID, Delta: variant.Message.ReasoningContent}); err != nil {
			return nil, false, err
		}
	}
	if err := emitEvent(ctx, sink, Event{Type: typeOfEvent, TurnID: turnID, Message: eventMessage}); err != nil {
		return nil, false, err
	}
	return variant.Message, false, nil
}

func consumeStreamingMessage(ctx context.Context, variant *adk.MessageVariant, turnID string, sink EventSink) (*schema.Message, bool, error) {
	if variant.MessageStream == nil {
		return nil, true, errors.New("streaming Agent output has no message stream")
	}
	stream := variant.MessageStream
	defer stream.Close()
	role := variant.Role
	chunks := make([]*schema.Message, 0, 8)
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, true, err
		}
		if chunk == nil {
			return nil, true, errors.New("Agent output stream returned a nil message")
		}
		chunks = append(chunks, chunk)
		if role == "" {
			role = chunk.Role
		}
		if role == schema.Assistant && chunk.ReasoningContent != "" {
			if err := emitEvent(ctx, sink, Event{Type: EventAssistantReasoningDelta, TurnID: turnID, Delta: chunk.ReasoningContent}); err != nil {
				return nil, true, err
			}
		}
		if chunk.Content != "" && role == schema.Assistant {
			if err := emitEvent(ctx, sink, Event{Type: EventAssistantTextDelta, TurnID: turnID, Delta: chunk.Content}); err != nil {
				return nil, true, err
			}
		}
	}
	message, err := schema.ConcatMessages(chunks)
	if err != nil {
		return nil, true, fmt.Errorf("concat Agent output stream: %w", err)
	}
	if message == nil {
		return nil, true, nil
	}
	typeOfEvent := EventAssistantMessage
	if role == schema.Tool || message.Role == schema.Tool {
		typeOfEvent = EventToolResult
	}
	eventMessage, err := cloneMessage(message)
	if err != nil {
		return nil, true, err
	}
	if err := emitEvent(ctx, sink, Event{Type: typeOfEvent, TurnID: turnID, Message: eventMessage}); err != nil {
		return nil, true, err
	}
	return message, true, nil
}

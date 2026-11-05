package conversation

import (
	"context"

	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
)

// ContextUsage reports the current context size using the Factory's model
// tokenizer and calibration state.
func (conversation *Conversation) ContextUsage() agentruntime.ContextUsage {
	conversation.mutex.Lock()
	defer conversation.mutex.Unlock()
	return conversation.factory.ContextUsage(conversation.history)
}

// ContextUsageFor includes a pending user input without persisting it. TUI
// callers use this to show the budget impact immediately after submission.
func (conversation *Conversation) ContextUsageFor(input string) agentruntime.ContextUsage {
	conversation.mutex.Lock()
	defer conversation.mutex.Unlock()
	messages := append(cloneMessages(conversation.history), schema.UserMessage(input))
	return conversation.factory.ContextUsage(messages)
}

// Compress forces the same durable compaction policy used automatically before
// a turn. It is safe to call while the UI is idle and refreshes the projection
// immediately after the compaction event is recorded.
func (conversation *Conversation) Compress(ctx context.Context) (bool, agentruntime.ContextUsage, error) {
	conversation.mutex.Lock()
	defer conversation.mutex.Unlock()
	if len(conversation.history) == 0 {
		return false, conversation.factory.ContextUsage(nil), nil
	}
	_, changed, err := conversation.factory.Compact(ctx, conversation.sessionID, cloneMessages(conversation.history))
	if err != nil {
		return false, conversation.factory.ContextUsage(conversation.history), err
	}
	if changed {
		if err := conversation.refreshHistory(ctx, "refresh session history after compaction"); err != nil {
			return false, conversation.factory.ContextUsage(conversation.history), err
		}
	}
	return changed, conversation.factory.ContextUsage(conversation.history), nil
}

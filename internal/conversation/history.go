package conversation

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cloudwego/eino/schema"
)

// refreshHistory replaces the projection only after a complete durable read.
// Callers hold the session mutex and supply the existing boundary-specific
// diagnostic. A failed refresh must not expose a partially updated history.
func (conversation *Conversation) refreshHistory(ctx context.Context, boundary string) error {
	history, err := conversation.factory.LoadHistory(ctx, conversation.sessionID)
	if err != nil {
		return fmt.Errorf("%s: %w", boundary, err)
	}
	conversation.history = history
	return nil
}

// History returns a stable snapshot of completed model messages.
func (conversation *Conversation) History() []*schema.Message {
	conversation.mutex.Lock()
	defer conversation.mutex.Unlock()
	return cloneMessages(conversation.history)
}

// cloneMessages makes the read-side snapshot genuinely immutable to callers.
// schema.Message contains slices and maps, so copying only the outer slice
// would let a UI mutate the conversation's internal model context.
func cloneMessages(messages []*schema.Message) []*schema.Message {
	if len(messages) == 0 {
		return nil
	}
	clones := make([]*schema.Message, len(messages))
	for index, message := range messages {
		if message == nil {
			continue
		}
		encoded, err := json.Marshal(message)
		if err != nil {
			// Messages loaded from the durable history are JSON-compatible. Keep
			// a nil entry rather than exposing the mutable original on failure.
			continue
		}
		var clone schema.Message
		if err := json.Unmarshal(encoded, &clone); err == nil {
			clones[index] = &clone
		}
	}
	return clones
}

// CompletedTurns returns the number of completed user turns in history.
func (conversation *Conversation) CompletedTurns() int {
	conversation.mutex.Lock()
	defer conversation.mutex.Unlock()
	return conversation.completedTurns
}

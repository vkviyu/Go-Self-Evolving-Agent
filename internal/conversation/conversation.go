// Package conversation owns session serialization and durable history views.
// Interfaces submit turns here; Eino owns the model/tool execution loop.
package conversation

import (
	"context"
	"errors"
	"sync"

	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

// Conversation serializes turns for one durable session.
type Conversation struct {
	factory   *agentruntime.Factory
	sessionID session.ID

	mutex          sync.Mutex
	history        []*schema.Message
	completedTurns int
}

// Open loads completed history before accepting new turns.
func Open(ctx context.Context, factory *agentruntime.Factory, sessionID session.ID) (*Conversation, error) {
	if factory == nil {
		return nil, errors.New("conversation requires an Agent factory")
	}
	if sessionID == "" {
		return nil, errors.New("conversation requires a session ID")
	}
	history, err := factory.LoadHistory(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	completedTurns, err := factory.CountCompletedUserTurns(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return &Conversation{factory: factory, sessionID: sessionID, history: history, completedTurns: completedTurns}, nil
}

// ID returns the durable session identifier.
func (conversation *Conversation) ID() session.ID {
	return conversation.sessionID
}

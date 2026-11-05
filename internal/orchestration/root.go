package orchestration

import (
	"context"
	"encoding/json"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

type RootThread struct {
	TeamID          TeamID
	AgentID         AgentID
	ThreadID        ThreadID
	SessionID       session.ID
	TeamState       TeamState
	AgentState      AgentState
	ProfileRevision string
	CreatedAt       time.Time
}

func (root RootThread) ActorScope() ActorScope {
	return ActorScope{
		TeamID:          root.TeamID,
		AgentID:         root.AgentID,
		ThreadID:        root.ThreadID,
		ProfileRevision: root.ProfileRevision,
	}
}

type RootThreadSeed struct {
	TeamID          TeamID
	AgentID         AgentID
	ThreadID        ThreadID
	SessionID       session.ID
	ProfileRevision string
	ProfileSnapshot json.RawMessage
	IdempotencyKey  string
}

type RootThreadRepository interface {
	EnsureRootThread(ctx context.Context, seed RootThreadSeed) (root RootThread, created bool, err error)
	RootThreadBySession(ctx context.Context, sessionID session.ID) (RootThread, error)
}

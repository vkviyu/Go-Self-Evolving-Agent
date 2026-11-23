package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

// TurnInput restores the trusted user input and the original start time for a
// durable turn. Recovery uses this data instead of reconstructing a request
// from model-visible text.
func (f *Factory) TurnInput(ctx context.Context, sessionID session.ID, turnID string) (string, time.Time, error) {
	var input string
	var started time.Time
	err := session.VisitEvents(ctx, f.events, sessionID, func(event session.Event) error {
		if event.CorrelationID != turnID {
			return nil
		}
		if event.Type == session.EventTurnStarted {
			started = event.OccurredAt
		}
		if event.Type == session.EventUserMessage {
			var payload struct {
				Content string `json:"content"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return err
			}
			input = payload.Content
		}
		return nil
	})
	if err == nil && (input == "" || started.IsZero()) {
		err = fmt.Errorf("trusted input for turn %q is unavailable", turnID)
	}
	return input, started, err
}

// CountCompletedUserTurns counts completed foreground user turns. Background
// wake turns carry an explicit input marker and are excluded.
func (f *Factory) CountCompletedUserTurns(ctx context.Context, sessionID session.ID) (int, error) {
	completed := make(map[string]bool)
	background := make(map[string]bool)
	err := session.VisitEvents(ctx, f.events, sessionID, func(event session.Event) error {
		switch event.Type {
		case session.EventTurnStarted:
			var payload struct {
				Input string `json:"input"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return fmt.Errorf("decode turn metadata: %w", err)
			}
			background[event.CorrelationID] = payload.Input == TurnInputBackground
		case session.EventTurnCompleted:
			completed[event.CorrelationID] = true
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	count := 0
	for turnID := range completed {
		if !background[turnID] {
			count++
		}
	}
	return count, nil
}

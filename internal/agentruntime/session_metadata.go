package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

// FirstUserPrompt reads the original naming source, independently of compacted
// model context and turn completion. Background messages never name a session.
// The UI derives a short display title; no model request or event write occurs.
func (f *Factory) FirstUserPrompt(ctx context.Context, id session.ID) (string, error) {
	if f == nil || f.events == nil || id == "" {
		return "", errors.New("session metadata requires an event store and session ID")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var after uint64
	for {
		events, err := f.events.Load(ctx, id, after, 64)
		if err != nil {
			return "", fmt.Errorf("read session naming source: %w", err)
		}
		if len(events) == 0 {
			return "", nil
		}
		for _, event := range events {
			if event.Sequence <= after {
				return "", errors.New("session metadata event sequence did not advance")
			}
			after = event.Sequence
			if event.Type != session.EventUserMessage {
				continue
			}
			var input struct {
				Content string `json:"content"`
			}
			if err := json.Unmarshal(event.Payload, &input); err != nil {
				return "", fmt.Errorf("decode session naming source: %w", err)
			}
			if strings.TrimSpace(input.Content) != "" {
				return input.Content, nil
			}
		}
	}
}

package session_test

import (
	"context"
	"errors"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
)

// Hide the optional indexed port to exercise paginated legacy readers.
type pagedEvents struct{ session.EventStore }

func TestRecoveryGateChecksIndexedAndPagedEvidence(t *testing.T) {
	ctx := context.Background()
	events := memorystore.New()
	items := make([]session.PendingEvent, 1001)
	for index := range items {
		items[index] = session.PendingEvent{Type: session.EventToolStarted, CorrelationID: "other", Payload: []byte(`{}`)}
	}
	items = append(items, session.PendingEvent{Type: session.EventTurnAbandoned, CorrelationID: "target", Payload: []byte(`{}`)})
	if _, err := events.Append(ctx, "session", session.AnySequence, items...); err != nil {
		t.Fatal(err)
	}
	for _, reader := range []session.EventStore{events, pagedEvents{events}} {
		if err := session.CheckRecovery(ctx, reader, "session", "target"); !errors.Is(err, session.ErrRecoveryAbandoned) {
			t.Fatalf("missed tombstone: %v", err)
		}
		if err := session.CheckRecovery(ctx, reader, "session", "other"); err != nil {
			t.Fatalf("blocked unrelated turn: %v", err)
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if err := session.CheckRecovery(canceled, reader, "session", "target"); !errors.Is(err, context.Canceled) {
			t.Fatalf("ignored canceled read: %v", err)
		}
	}
}

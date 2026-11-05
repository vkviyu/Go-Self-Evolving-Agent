package session_test

import (
	"context"
	"errors"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
)

func TestVisitEventsPagesStopsAndPropagates(t *testing.T) {
	ctx := context.Background()
	events := memorystore.New()
	items := make([]session.PendingEvent, 2500)
	for index := range items {
		items[index] = session.PendingEvent{Type: session.EventToolStarted, Payload: []byte(`{}`)}
	}
	if _, err := events.Append(ctx, "session", session.AnySequence, items...); err != nil {
		t.Fatal(err)
	}
	var seen uint64
	if err := session.VisitEvents(ctx, events, "session", func(event session.Event) error {
		if event.Sequence != seen+1 {
			t.Fatalf("sequence %d after %d", event.Sequence, seen)
		}
		seen = event.Sequence
		return nil
	}); err != nil || seen != 2500 {
		t.Fatalf("visited %d events: %v", seen, err)
	}

	seen = 0
	if err := session.VisitEvents(ctx, events, "session", func(event session.Event) error {
		seen = event.Sequence
		if seen == 1200 {
			return session.ErrStopVisit
		}
		return nil
	}); err != nil || seen != 1200 {
		t.Fatalf("early stop at %d: %v", seen, err)
	}

	failure := errors.New("visitor failed")
	if err := session.VisitEvents(ctx, events, "session", func(session.Event) error { return failure }); !errors.Is(err, failure) {
		t.Fatalf("visitor error = %v", err)
	}
	if err := session.VisitEvents(ctx, events, "missing", func(session.Event) error {
		t.Fatal("visited an event of an empty session")
		return nil
	}); err != nil {
		t.Fatalf("empty session: %v", err)
	}
}

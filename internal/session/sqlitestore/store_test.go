package sqlitestore

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

func TestStoreAppendLoadAndSequenceConflict(t *testing.T) {
	t.Parallel()

	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "state", "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})

	sessionID := session.ID("session-1")
	appended, err := store.SessionEvents().Append(context.Background(), sessionID, 0,
		session.PendingEvent{Type: session.EventSessionCreated, Payload: session.MustJSONPayload(map[string]string{"source": "test"})},
		session.PendingEvent{Type: session.EventUserMessage, Payload: session.MustJSONPayload(map[string]string{"content": "hello"})},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(appended), 2; got != want {
		t.Fatalf("appended %d events, want %d", got, want)
	}
	if appended[0].Sequence != 1 || appended[1].Sequence != 2 {
		t.Fatalf("unexpected sequences: %d, %d", appended[0].Sequence, appended[1].Sequence)
	}

	_, err = store.SessionEvents().Append(context.Background(), sessionID, 0,
		session.PendingEvent{Type: session.EventTurnStarted, Payload: session.MustJSONPayload(map[string]string{})},
	)
	if !errors.Is(err, session.ErrSequenceConflict) {
		t.Fatalf("got %v, want ErrSequenceConflict", err)
	}

	events, err := store.SessionEvents().Load(context.Background(), sessionID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(events), 2; got != want {
		t.Fatalf("loaded %d events, want %d", got, want)
	}
	if events[1].Type != session.EventUserMessage {
		t.Fatalf("last event type = %q", events[1].Type)
	}
	if events[0].Checksum == "" || events[1].Checksum == "" {
		t.Fatal("checksums must be populated")
	}

	current, err := store.SessionEvents().CurrentSequence(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if current != 2 {
		t.Fatalf("current sequence = %d, want 2", current)
	}
}

func TestStoreRejectsInvalidPayload(t *testing.T) {
	t.Parallel()

	store, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	_, err = store.SessionEvents().Append(context.Background(), "session-1", session.AnySequence,
		session.PendingEvent{Type: session.EventUserMessage, Payload: []byte("not-json")},
	)
	if !errors.Is(err, session.ErrInvalidEvent) {
		t.Fatalf("got %v, want ErrInvalidEvent", err)
	}
}

func TestStoreRejectsEmptyPayload(t *testing.T) {
	t.Parallel()

	store, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	_, err = store.SessionEvents().Append(context.Background(), "session-1", session.AnySequence,
		session.PendingEvent{Type: session.EventUserMessage},
	)
	if !errors.Is(err, session.ErrInvalidEvent) {
		t.Fatalf("got %v, want ErrInvalidEvent", err)
	}
}

func TestStoreCheckpointLifecycle(t *testing.T) {
	store, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Checkpoints().Set(context.Background(), "turn-1", []byte("checkpoint")); err != nil {
		t.Fatal(err)
	}
	data, exists, err := store.Checkpoints().Get(context.Background(), "turn-1")
	if err != nil || !exists || string(data) != "checkpoint" {
		t.Fatalf("checkpoint = %q, exists=%v, err=%v", data, exists, err)
	}
	if err := store.Checkpoints().Delete(context.Background(), "turn-1"); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := store.Checkpoints().Get(context.Background(), "turn-1"); err != nil || exists {
		t.Fatalf("checkpoint still exists: exists=%v, err=%v", exists, err)
	}
}

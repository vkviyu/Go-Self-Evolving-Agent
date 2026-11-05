package agentruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
)

func TestSessionNamingSourceSurvivesCompactionAndFailedFirstTurn(t *testing.T) {
	ctx, store := context.Background(), memorystore.New()
	// Exercise pagination and do not mistake Agent background for a user query.
	pending := make([]session.PendingEvent, 70)
	for index := range pending {
		pending[index] = session.PendingEvent{Type: session.EventBackgroundInput, Payload: session.MustJSONPayload(map[string]any{"content": "Agent completed work"})}
	}
	first := "最初的问题：修复终端中文换行\n保留完整尾句"
	pending = append(pending,
		session.PendingEvent{Type: session.EventUserMessage, Payload: session.MustJSONPayload(map[string]any{"content": first})},
		session.PendingEvent{Type: session.EventTurnFailed, Payload: session.MustJSONPayload(map[string]any{"error": "provider unavailable"})},
		session.PendingEvent{Type: session.EventContextCompacted, Payload: session.MustJSONPayload(map[string]any{"summary": "shortened context"})},
		session.PendingEvent{Type: session.EventUserMessage, Payload: session.MustJSONPayload(map[string]any{"content": "后来的问题"})},
	)
	if _, err := store.Append(ctx, "title", session.AnySequence, pending...); err != nil {
		t.Fatal(err)
	}
	factory := &Factory{events: store}
	got, err := factory.FirstUserPrompt(ctx, "title")
	if err != nil || got != first {
		t.Fatalf("naming source=%q err=%v", got, err)
	}
	sequence, err := store.CurrentSequence(ctx, "title")
	if err != nil || sequence != uint64(len(pending)) {
		t.Fatalf("metadata lookup wrote events: sequence=%d err=%v", sequence, err)
	}
}

func TestSessionNamingSourceEmptyAndCanceled(t *testing.T) {
	store := memorystore.New()
	factory := &Factory{events: store}
	if got, err := factory.FirstUserPrompt(context.Background(), "new"); err != nil || got != "" {
		t.Fatalf("new session naming source=%q err=%v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := factory.FirstUserPrompt(ctx, "new"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled metadata lookup: %v", err)
	}
}

package agentruntime

import (
	"strings"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
)

func TestInterruptedContextRetainsCancellationAndUnconfirmedOutcome(t *testing.T) {
	ctx, store := t.Context(), memorystore.New()
	factory := checkpointFactory(t, store)
	const id session.ID = "interrupted-work"
	add := func(kind session.EventType, ref string, payload any) {
		t.Helper()
		if _, err := store.Append(ctx, id, session.AnySequence, session.PendingEvent{
			Type: kind, CorrelationID: ref, Payload: session.MustJSONPayload(payload),
		}); err != nil {
			t.Fatal(err)
		}
	}
	add(session.EventTurnStarted, "original", map[string]bool{"foreground": true})
	add(session.EventUserMessage, "original", map[string]string{"content": "Update the workspace inventory"})
	add(session.EventAssistantMessage, "original", map[string]string{"content": "The inventory needs one more entry"})
	add(session.EventToolProposed, "write-1", map[string]any{
		"turn_id": "original", "call_id": "write-1", "tool": "workspace_write", "arguments": map[string]string{"path": "inventory.txt"},
	})
	add(session.EventToolStarted, "write-1", map[string]string{"turn_id": "original", "call_id": "write-1", "tool": "workspace_write"})
	add(session.EventTurnFailed, "original", map[string]string{"stop_kind": "cancelled", "error": "internal diagnostic"})
	add(session.EventTurnStarted, "wake", map[string]string{"input": TurnInputBackground})
	add(session.EventBackgroundInput, "wake", map[string]string{"content": "unrelated mailbox"})
	add(session.EventTurnCompleted, "wake", struct{}{})

	view, err := factory.LoadInterruptedContext(ctx, id)
	if err != nil || view == nil {
		t.Fatalf("missing interruption context: %v", err)
	}
	for _, want := range []string{"Update the workspace inventory", "cancelled", "outcome_unconfirmed", "inventory.txt", "The inventory needs one more entry"} {
		if !strings.Contains(view.Content, want) {
			t.Errorf("missing %q in %s", want, view.Content)
		}
	}
	for _, hidden := range []string{"internal diagnostic", "unrelated mailbox"} {
		if strings.Contains(view.Content, hidden) {
			t.Errorf("unexpected %q in %s", hidden, view.Content)
		}
	}
	history, err := factory.LoadHistory(ctx, id)
	if err != nil || len(history) != 0 {
		t.Fatalf("interrupted work entered committed history: %v %v", history, err)
	}
}

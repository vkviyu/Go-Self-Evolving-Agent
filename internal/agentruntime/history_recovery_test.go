package agentruntime

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
)

func TestUnversionedContextCheckpointIsRejected(t *testing.T) {
	store := memorystore.New()
	factory := checkpointFactory(t, store)
	if _, err := store.Append(t.Context(), "unsupported", session.AnySequence, session.PendingEvent{
		Type:    session.EventContextCompacted,
		Payload: session.MustJSONPayload(map[string]string{"summary": "unversioned"}),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := factory.LoadHistory(t.Context(), "unsupported"); err == nil || !strings.Contains(err.Error(), "unsupported context checkpoint version 0") {
		t.Fatalf("unversioned checkpoint was accepted: %v", err)
	}
}

func TestResumedTurnCannotPublishAStaleContextCheckpoint(t *testing.T) {
	for _, mode := range []string{"newer turn", "newer compacted turn", "manual compaction"} {
		t.Run(mode, func(t *testing.T) {
			ctx, store := t.Context(), memorystore.New()
			factory := checkpointFactory(t, store)
			const id session.ID = "recovery-projection"
			appendEvent := func(kind session.EventType, turn string, payload any) {
				t.Helper()
				if _, err := store.Append(ctx, id, session.AnySequence, session.PendingEvent{Type: kind, CorrelationID: turn, Payload: session.MustJSONPayload(payload)}); err != nil {
					t.Fatal(err)
				}
			}
			start := func(turn, input string) {
				appendEvent(session.EventTurnStarted, turn, struct{}{})
				appendEvent(session.EventUserMessage, turn, map[string]string{"content": input})
			}
			checkpoint := func(turn, summary string, retained ...*schema.Message) {
				appendEvent(session.EventContextCompacted, turn, historyContextCheckpoint{TurnID: turn, Version: 2, Summary: summary, Retained: retained})
			}
			complete := func(turn, answer string) {
				appendEvent(session.EventAssistantMessage, turn, map[string]string{"content": answer})
				appendEvent(session.EventTurnCompleted, turn, struct{}{})
			}

			start("a", "A question")
			checkpoint("a", "A stale summary", schema.UserMessage("A question"))
			appendEvent(session.EventTurnFailed, "a", map[string]string{"error": "provider disconnected"})
			start("b", "B question")
			want := []*schema.Message{schema.UserMessage("B question"), schema.AssistantMessage("B answer", nil)}
			if mode == "newer compacted turn" {
				checkpoint("b", "B summary", want[0])
				want = append([]*schema.Message{schema.SystemMessage("Conversation summary:\nB summary")}, want...)
			}
			complete("b", "B answer")
			if mode == "manual compaction" {
				appendEvent(session.EventContextCompacted, "manual", historyContextCheckpoint{Version: 1, Summary: "Manual summary", Retained: want})
				want = append([]*schema.Message{schema.SystemMessage("Conversation summary:\nManual summary")}, want...)
			}
			before, err := factory.LoadHistory(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			assertCheckpointMessages(t, want, before)
			transcript, err := factory.LoadTranscript(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			assertCheckpointMessages(t, []*schema.Message{
				schema.UserMessage("A question"),
				schema.UserMessage("B question"),
				schema.AssistantMessage("B answer", nil),
			}, transcript)

			// A can also compact again after resuming. Its original input still
			// predates B, regardless of when this new checkpoint was recorded.
			if mode == "newer turn" {
				checkpoint("a", "A stale summary after resume", schema.UserMessage("A question"))
			}
			complete("a", "A resumed answer")
			after, err := factory.LoadHistory(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			want = append(want, schema.UserMessage("A question"), schema.AssistantMessage("A resumed answer", nil))
			assertCheckpointMessages(t, want, after)
		})
	}
}

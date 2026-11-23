package agentruntime

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenizer"
)

func TestLoadTranscriptKeepsOpenTurnAndSkipsCalibration(t *testing.T) {
	ctx, store := context.Background(), memorystore.New()
	factory := profileFactory(t, store, &contextLoopModel{})
	factory.tokenizer = tokenizer.NewApproximate("test")
	input := []*schema.Message{schema.UserMessage("hello")}
	estimated := factory.tokenizer.CountMessages(input)
	event := func(kind session.EventType, turnID string, payload map[string]any) session.PendingEvent {
		return session.PendingEvent{Type: kind, CorrelationID: turnID, Payload: session.MustJSONPayload(payload)}
	}
	_, err := store.Append(ctx, "child", session.AnySequence,
		event(session.EventUserMessage, "done", map[string]any{"content": "hello"}),
		event(session.EventModelRequested, "done", map[string]any{"request_id": "done:1", "estimated_input_tokens": estimated}),
		event(session.EventAssistantMessage, "done", map[string]any{"request_id": "done:1", "content": "reply", "prompt_tokens": estimated + 100}),
		event(session.EventTurnCompleted, "done", map[string]any{}),
		// The second turn is still running: it has no completion event.
		event(session.EventUserMessage, "open", map[string]any{"content": "inspect the parser"}),
		event(session.EventAssistantMessage, "open", map[string]any{"content": "reading parser.go"}),
	)
	if err != nil {
		t.Fatal(err)
	}

	transcript, err := factory.LoadTranscript(ctx, "child")
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript) != 4 || transcript[2].Content != "inspect the parser" || transcript[3].Content != "reading parser.go" {
		t.Fatalf("transcript lost the open turn: %#v", transcript)
	}
	if got := factory.tokenizer.CountMessages(input); got != estimated {
		t.Fatalf("transcript calibrated the tokenizer: got=%d want=%d", got, estimated)
	}

	history, err := factory.LoadHistory(ctx, "child")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[1].Content != "reply" {
		t.Fatalf("model history kept the open turn: %#v", history)
	}
}

func TestTranscriptRevisionTracksStreamingChildOutput(t *testing.T) {
	ctx, store := t.Context(), memorystore.New()
	factory := profileFactory(t, store, &contextLoopModel{})
	const child session.ID = "child"
	const turnID = "open"
	const requestID = "open:1"
	appendEvent := func(kind session.EventType, payload map[string]any) {
		t.Helper()
		if _, err := store.Append(ctx, child, session.AnySequence, session.PendingEvent{
			Type: kind, CorrelationID: turnID, Payload: session.MustJSONPayload(payload),
		}); err != nil {
			t.Fatal(err)
		}
	}
	checkRevision := func(want uint64) {
		t.Helper()
		if got, err := factory.TranscriptRevision(ctx, child); err != nil || got != want {
			t.Fatalf("transcript revision = %d, %v, want %d", got, err, want)
		}
	}
	checkAssistant := func(want string) {
		t.Helper()
		transcript, err := factory.LoadTranscript(ctx, child)
		if err != nil {
			t.Fatal(err)
		}
		if len(transcript) != 2 || transcript[1].Role != schema.Assistant || transcript[1].Content != want {
			t.Fatalf("unexpected transcript %#v, want assistant text %q", transcript, want)
		}
	}

	checkRevision(0)
	appendEvent(session.EventUserMessage, map[string]any{"content": "inspect the parser"})
	appendEvent(session.EventModelPartial, map[string]any{
		"request_id": requestID, "chunk": schema.AssistantMessage("Reading", nil),
	})
	checkRevision(2)
	checkAssistant("Reading\n[Partial model response]")

	appendEvent(session.EventModelPartial, map[string]any{
		"request_id": requestID, "chunk": schema.AssistantMessage(" parser.go", nil),
	})
	checkRevision(3)
	checkAssistant("Reading parser.go\n[Partial model response]")

	appendEvent(session.EventAssistantMessage, map[string]any{"request_id": requestID, "content": "Reading parser.go"})
	checkRevision(4)
	checkAssistant("Reading parser.go")
}

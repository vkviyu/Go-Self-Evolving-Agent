package agentruntime

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenizer"
)

func TestFollowupRecoversVisibleReplyOmittedFromCompletedOpening(t *testing.T) {
	const id session.ID = "focus"
	ctx, store := t.Context(), memorystore.New()
	factory := checkpointFactory(t, store)
	factory.config.ContextWindowTokens = 32768
	factory.tokenizer = tokenizer.NewApproximate("gateway")
	factory.tokenizer.ObserveProviderInput(15009, 10000)
	add := func(turn string, kind session.EventType, payload any) {
		t.Helper()
		_, err := store.Append(ctx, id, session.AnySequence, session.PendingEvent{
			Type: kind, CorrelationID: turn, Payload: session.MustJSONPayload(payload),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	start := func(turn string, input ...*schema.Message) {
		add(turn, session.EventTurnStarted, map[string]any{"foreground": true, "journal": turnJournal{Version: 1, Input: input}})
		add(turn, session.EventUserMessage, map[string]string{"content": input[len(input)-1].Content})
	}
	done := func(turn string) { add(turn, session.EventTurnCompleted, struct{}{}) }
	start("count", schema.UserMessage("count source lines"))
	done("count")
	start("explain", schema.UserMessage("explain the observer"))
	add("explain", session.EventModelRequested, map[string]string{"request_id": "explain:1"})
	add("explain", session.EventModelPartial, map[string]any{"request_id": "explain:1", "chunk": schema.AssistantMessage("Recorder feeds writer", nil)})
	add("explain", session.EventModelFailed, map[string]string{"request_id": "explain:1"})
	load := func() *schema.Message {
		t.Helper()
		view, err := factory.LoadInterruptedContext(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return view
	}
	follow := schema.UserMessage("go on")
	view := load()
	if view == nil || !strings.Contains(view.Content, "Recorder feeds writer") {
		t.Fatalf("calibration discarded the active reply: %v", view)
	}
	// Reproduce an older host which completed an unrelated response without
	// ever giving the model the immediately preceding unfinished explanation.
	start("missed", schema.UserMessage("count source lines"), schema.AssistantMessage("100 lines", nil), follow)
	done("missed")
	view = load()
	if view == nil || !strings.Contains(view.Content, "explain the observer") || !strings.Contains(view.Content, "omitted_from_followup") || strings.Contains(view.Content, "count source lines") {
		t.Fatalf("missing or unrelated prior work: %v", view)
	}
	start("informed", view, follow)
	done("informed")
	if view = load(); view != nil {
		t.Fatalf("prior reply resurfaced after informed completion: %v", view)
	}
}

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

func TestInterruptedReplyUsesVisibleTextAcrossRetries(t *testing.T) {
	var turn interruptedTurn
	apply := func(kind session.EventType, payload any) {
		t.Helper()
		if err := turn.apply(session.Event{Type: kind, Payload: session.MustJSONPayload(payload)}); err != nil {
			t.Fatal(err)
		}
	}
	start := func(id string) {
		apply(session.EventModelRequested, map[string]string{"request_id": id})
	}
	delta := func(id, text string) {
		apply(session.EventModelPartial, map[string]any{"request_id": id, "chunk": map[string]any{
			"content": text, "reasoning_content": "private", "tool_calls": []any{map[string]any{"function": map[string]string{"arguments": "fragment"}}},
		}})
	}
	start("first")
	delta("first", "上半句")
	delta("first", "，下半")
	// A retry that fails before delivering visible text leaves the user's
	// stopping point intact, even if it produced reasoning or tool arguments.
	start("retry")
	delta("retry", "")
	delta("first", "late delta")
	if got := turn.draft.tail.String(); got != "上半句，下半" || turn.draft.requestID != "first" {
		t.Fatalf("wrong stopping point after empty retry: %q", got)
	}
	delta("retry", "新回答")
	delta("retry", "的中间")
	if got := turn.draft.tail.String(); got != "新回答的中间" {
		t.Fatalf("mixed response attempts: %q", got)
	}
	apply(session.EventAssistantMessage, map[string]string{"request_id": "retry", "content": "完整回答"})
	start("next")
	delta("next", "")
	if turn.draft.tail.Len() != 0 || turn.assistant != "完整回答" {
		t.Fatal("a completed reply resurfaced as unfinished")
	}
}

func TestInterruptedReplyKeepsUnicodeEndingWithinBudget(t *testing.T) {
	factory := checkpointFactory(t, memorystore.New())
	factory.config.ContextWindowTokens = 8192
	factory.tokenizer = tokenizer.NewApproximate("test")
	record := TurnRecord{TurnID: "turn"}
	turn := &interruptedTurn{record: record, request: "解释这份报告", calls: map[string]*interruptedTool{
		"write": {Turn: "turn", CallID: "write", Name: "workspace_write", Status: "outcome_unconfirmed"},
	}}
	turn.draft.append("r", "早期文字"+strings.Repeat("甲乙丙", interruptedContextMaxBytes/9+1))
	for range 400 {
		turn.draft.append("r", "，丁")
	}
	turn.draft.append("r", "【实际中断处】")
	if tail := turn.draft.tail.String(); !utf8.ValidString(tail) || len(tail) > interruptedContextMaxBytes || strings.Contains(tail, "早期文字") {
		t.Fatalf("invalid retained suffix (%d bytes)", len(tail))
	}
	view := factory.interruptedContextMessage([]TurnRecord{record}, map[string]*interruptedTurn{"turn": turn})
	if view == nil || !strings.Contains(view.Content, "【实际中断处】") || !strings.Contains(view.Content, "outcome_unconfirmed") ||
		strings.Contains(view.Content, turn.draft.tail.String()) || strings.ContainsRune(view.Content, utf8.RuneError) {
		t.Fatalf("lost the interruption or tool state: %v", view)
	}
}

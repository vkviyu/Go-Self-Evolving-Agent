package agentruntime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func checkpointFactory(t *testing.T, store *memorystore.Store) *Factory {
	t.Helper()
	registry := tool.NewRegistry()
	engine, err := policy.DefaultDeny()
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := tool.NewPipeline(tool.PipelineConfig{Registry: registry, Policy: engine, Events: store})
	if err != nil {
		t.Fatal(err)
	}
	factory, err := NewFactory(Config{ContextWindowTokens: 1000, CompactionTriggerTokens: 800, Tokenizer: fixedContextCounter{}}, &summaryModel{}, registry, pipeline, store, store)
	if err != nil {
		t.Fatal(err)
	}
	return factory
}

func TestCompactionCheckpointRestoresRetainedMessages(t *testing.T) {
	ctx := context.Background()
	store := memorystore.New()
	factory := checkpointFactory(t, store)
	history := []*schema.Message{
		schema.UserMessage("old"), schema.AssistantMessage("old answer", nil),
		schema.UserMessage("keep this"), schema.AssistantMessage("exact recent answer", nil),
	}
	for attempt := range 2 {
		prepared, changed, _, err := factory.compactWithResult(ctx, "restore", history, 60, contextLevelElevated, ContextHints{}, 0)
		if err != nil || changed != (attempt == 0) {
			t.Fatalf("compact changed=%v err=%v", changed, err)
		}
		factory = checkpointFactory(t, store)
		reloaded, err := factory.LoadHistory(ctx, "restore")
		if err != nil {
			t.Fatal(err)
		}
		assertCheckpointMessages(t, prepared, reloaded)
		if len(reloaded) != 3 || reloaded[1].Content != "keep this" {
			t.Fatalf("lost suffix: %#v", reloaded)
		}
		history = reloaded
	}
}

func TestCompactionCheckpointDoesNotDuplicatePendingInput(t *testing.T) {
	ctx := context.Background()
	store := memorystore.New()
	factory := checkpointFactory(t, store)
	prepared, _, _, err := factory.compactWithResult(ctx, "pending", []*schema.Message{
		schema.UserMessage("old"), schema.AssistantMessage("old answer", nil),
		schema.UserMessage("recent"), schema.AssistantMessage("retained answer", nil), schema.UserMessage("pending"),
	}, 90, contextLevelElevated, ContextHints{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	history, err := factory.LoadHistory(ctx, "pending")
	if err != nil {
		t.Fatal(err)
	}
	assertCheckpointMessages(t, prepared[:len(prepared)-1], history)
	appendEvent := func(kind session.EventType, payload any) {
		t.Helper()
		if _, err := store.Append(ctx, "pending", session.AnySequence, session.PendingEvent{Type: kind, CorrelationID: "new-turn", Payload: session.MustJSONPayload(payload)}); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent(session.EventTurnStarted, map[string]any{})
	appendEvent(session.EventUserMessage, map[string]any{"content": "pending"})
	interrupted, err := factory.LoadHistory(ctx, "pending")
	if err != nil {
		t.Fatal(err)
	}
	assertCheckpointMessages(t, history, interrupted)
	appendEvent(session.EventAssistantMessage, map[string]any{"content": "new answer"})
	appendEvent(session.EventTurnCompleted, map[string]any{})
	completed, err := factory.LoadHistory(ctx, "pending")
	if err != nil {
		t.Fatal(err)
	}
	assertCheckpointMessages(t, append(prepared, schema.AssistantMessage("new answer", nil)), completed)
}

func TestCompactionSuffixKeepsEntireToolGroup(t *testing.T) {
	messages := []*schema.Message{
		schema.UserMessage("old"),
		schema.AssistantMessage("", []schema.ToolCall{
			{ID: "a", Type: "function", Function: schema.FunctionCall{Name: "read", Arguments: "{}"}},
			{ID: "b", Type: "function", Function: schema.FunctionCall{Name: "read", Arguments: "{}"}},
		}), schema.ToolMessage("first", "a"), schema.ToolMessage("second", "b"),
	}
	for _, tokens := range []int{0, 1, 30, 60, 90} {
		keep := latestMessageCountWithCounter(fixedContextCounter{}, messages, tokens)
		if keep != 3 {
			t.Fatalf("budget=%d retained=%d; tool group split", tokens, keep)
		}
	}
}

func assertCheckpointMessages(t *testing.T, want, got []*schema.Message) {
	t.Helper()
	wantJSON, _ := json.Marshal(want)
	gotJSON, _ := json.Marshal(got)
	if string(wantJSON) != string(gotJSON) {
		t.Fatalf("checkpoint mismatch\nwant %s\ngot  %s", wantJSON, gotJSON)
	}
}

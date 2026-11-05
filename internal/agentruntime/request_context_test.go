package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/modelcontext"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenizer"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

type contextLoopModel struct {
	summaries atomic.Int32
	calls     atomic.Int32
	inputs    [][]*schema.Message
}

func (m *contextLoopModel) Generate(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	if len(input) > 0 && strings.HasPrefix(input[0].Content, compactionInstruction) {
		m.summaries.Add(1)
		return schema.AssistantMessage(testSemanticSummary, nil), nil
	}
	m.inputs = append(m.inputs, input)
	call := m.calls.Add(1)
	if call <= 2 {
		return schema.AssistantMessage("", []schema.ToolCall{{ID: string(rune('a' + call)), Type: "function", Function: schema.FunctionCall{Name: "read", Arguments: `{}`}}}), nil
	}
	return schema.AssistantMessage("finished", nil), nil
}

func (m *contextLoopModel) Stream(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := m.Generate(ctx, input, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func TestEinoCompactsDuringToolLoopAndReloadsOnlyCompletedContext(t *testing.T) {
	ctx := context.Background()
	store, fake := memorystore.New(), &contextLoopModel{}
	var toolCalls atomic.Int32
	factory := profileFactory(t, store, fake, profileBinding("read", tool.RiskRead, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		if toolCalls.Add(1) == 1 {
			return json.Marshal(map[string]string{"content": strings.Repeat("read output ", 450)})
		}
		return json.RawMessage(`{"content":"small"}`), nil
	}))
	factory.config.Instruction = "Preserve this host instruction."
	factory.config.ContextWindowTokens, factory.config.CompactionTriggerTokens, factory.config.MaxOutputTokens = 8000, 1800, 256
	factory.tokenizer = tokenizer.NewApproximate("test")
	turn, err := factory.NewTurn(ctx, "context-loop")
	if err != nil {
		t.Fatal(err)
	}
	result, err := turn.Run(ctx, []*schema.Message{
		schema.UserMessage(strings.Repeat("old question ", 130)),
		schema.AssistantMessage(strings.Repeat("old findings ", 130), nil),
		schema.UserMessage("Inspect the file and explain it; do not modify anything."),
	}, nil)
	if err != nil || result.Final == nil || result.Final.Content != "finished" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if fake.summaries.Load() != 1 || toolCalls.Load() != 2 || fake.calls.Load() != 3 {
		t.Fatalf("compaction repeated or loop failed: summaries=%d tools=%d models=%d", fake.summaries.Load(), toolCalls.Load(), fake.calls.Load())
	}
	for _, input := range fake.inputs[1:] {
		text, _ := json.Marshal(input)
		for _, want := range []string{"Preserve this host instruction.", "Conversation summary:", "do not modify anything"} {
			if !strings.Contains(string(text), want) {
				t.Errorf("compacted request lost %q: %s", want, text)
			}
		}
		if strings.Contains(string(text), "old findings old findings") {
			t.Fatal("Eino reintroduced compacted history on the next iteration")
		}
	}
	history, err := factory.LoadHistory(ctx, "context-loop")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(history)
	if !strings.Contains(string(encoded), "Conversation summary:") || strings.Count(string(encoded), "Inspect the file and explain it") != 1 || strings.Contains(string(encoded), "old findings old findings") || history[len(history)-1].Content != "finished" {
		t.Fatalf("invalid durable compacted history: %s", encoded)
	}
}

func TestRequestCheckpointDoesNotPublishInterruptedTurn(t *testing.T) {
	ctx, store := context.Background(), memorystore.New()
	factory := checkpointFactory(t, store)
	appendEvent := func(kind session.EventType, turn string, payload any) {
		t.Helper()
		if _, err := store.Append(ctx, "partial-context", session.AnySequence, session.PendingEvent{Type: kind, CorrelationID: turn, Payload: session.MustJSONPayload(payload)}); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent(session.EventUserMessage, "old", map[string]any{"content": "committed request"})
	appendEvent(session.EventAssistantMessage, "old", map[string]any{"content": "committed answer"})
	appendEvent(session.EventTurnCompleted, "old", map[string]any{})
	appendEvent(session.EventTurnStarted, "partial", map[string]any{})
	appendEvent(session.EventUserMessage, "partial", map[string]any{"content": "pending request"})
	appendEvent(session.EventContextCompacted, "partial", map[string]any{"context_checkpoint_version": 2, "turn_id": "partial", "summary": "partial summary", "retained_messages": []*schema.Message{schema.UserMessage("pending request")}})
	appendEvent(session.EventAssistantMessage, "partial", map[string]any{"content": "new answer"})
	history, err := factory.LoadHistory(ctx, "partial-context")
	if err != nil || len(history) != 2 || history[0].Content != "committed request" {
		t.Fatalf("interrupted context leaked: %+v %v", history, err)
	}
	appendEvent(session.EventTurnCompleted, "partial", map[string]any{})
	history, err = factory.LoadHistory(ctx, "partial-context")
	if err != nil || len(history) != 3 || history[0].Content != "Conversation summary:\npartial summary" || history[2].Content != "new answer" {
		t.Fatalf("completed checkpoint not applied: %+v %v", history, err)
	}
}

func TestOversizedLatestInputAndToolSchemasAreRejectedBeforeDispatch(t *testing.T) {
	for _, withTools := range []bool{false, true} {
		store, fake := memorystore.New(), &contextLoopModel{}
		factory := profileFactory(t, store, fake)
		factory.config.ContextWindowTokens, factory.config.CompactionTriggerTokens, factory.config.MaxOutputTokens = 1000, 600, 256
		factory.tokenizer = tokenizer.NewApproximate("test")
		recorded := &recordingModel{factory: factory, inner: fake, events: store, sessionID: "overflow", turnID: "turn"}
		input := []*schema.Message{schema.UserMessage(strings.Repeat("large input ", 1000))}
		options := []model.Option{model.WithMaxTokens(256)}
		if withTools {
			input = []*schema.Message{schema.UserMessage("hello")}
			options = append(options, model.WithTools([]*schema.ToolInfo{{Name: "large_schema", Desc: strings.Repeat("large tool description ", 1000)}}))
		}
		_, err := recorded.Generate(context.Background(), input, options...)
		if !errors.Is(err, ErrContextCapacity) || fake.calls.Load() != 0 || fake.summaries.Load() != 0 {
			t.Fatalf("tools=%v oversized request dispatched: %v calls=%d summaries=%d", withTools, err, fake.calls.Load(), fake.summaries.Load())
		}
	}
}

func TestFactoryContextCapacityIsAutomaticAndConfigurable(t *testing.T) {
	store := memorystore.New()
	factory := profileFactory(t, store, &contextLoopModel{})
	for _, explicit := range []int{0, 64000} {
		config := Config{Provider: "openai", Model: "gpt-5.6-sol", ContextWindowTokens: explicit, MaxOutputTokens: 8192}
		selected, err := NewFactory(config, factory.model, factory.registry, factory.pipeline, store, store)
		if err != nil {
			t.Fatal(err)
		}
		usage := selected.ContextUsage(nil)
		wantWindow, wantSource := 1050000, modelcontext.SourceCatalog
		if explicit > 0 {
			wantWindow, wantSource = explicit, modelcontext.SourceConfigured
		}
		if usage.Window != wantWindow || usage.WindowSource != wantSource || usage.Trigger+usage.OutputReserve >= usage.Window {
			t.Fatalf("explicit=%d context=%+v", explicit, usage)
		}
	}
}

func TestProviderCalibrationDoesNotDriftWhenHistoryIsRefreshed(t *testing.T) {
	ctx, store := context.Background(), memorystore.New()
	factory := profileFactory(t, store, &contextLoopModel{})
	factory.tokenizer = tokenizer.NewApproximate("test")
	input := []*schema.Message{schema.UserMessage("hello")}
	estimated := factory.tokenizer.CountMessages(input)
	actual := estimated + 100
	factory.observeProviderInput("turn:1", actual, estimated)
	_, err := store.Append(ctx, "calibration", session.AnySequence,
		session.PendingEvent{Type: session.EventUserMessage, CorrelationID: "turn", Payload: session.MustJSONPayload(map[string]any{"content": "hello"})},
		session.PendingEvent{Type: session.EventModelRequested, CorrelationID: "turn", Payload: session.MustJSONPayload(map[string]any{"request_id": "turn:1", "estimated_input_tokens": estimated})},
		session.PendingEvent{Type: session.EventAssistantMessage, CorrelationID: "turn", Payload: session.MustJSONPayload(map[string]any{"request_id": "turn:1", "content": "reply", "prompt_tokens": actual})},
		session.PendingEvent{Type: session.EventTurnCompleted, CorrelationID: "turn", Payload: session.MustJSONPayload(map[string]any{})},
	)
	if err != nil {
		t.Fatal(err)
	}
	for count := range 4 {
		if _, err := factory.LoadHistory(ctx, "calibration"); err != nil {
			t.Fatal(err)
		}
		if got := factory.tokenizer.CountMessages(input); got != actual {
			t.Fatalf("history refresh %d recalibrated the same usage: got=%d want=%d", count, got, actual)
		}
	}
}

func TestExternalizedToolOutputStaysOutOfReloadedContext(t *testing.T) {
	ctx, store := context.Background(), memorystore.New()
	fake, artifacts := &contextLoopModel{}, &memoryContextArtifacts{}
	factory := profileFactory(t, store, fake, profileBinding("read", tool.RiskRead, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return json.Marshal(map[string]string{"content": strings.Repeat("large tool output ", 2500)})
	}))
	factory.artifacts = artifacts
	factory.tokenizer = tokenizer.NewApproximate("test")
	factory.config.ContextWindowTokens, factory.config.CompactionTriggerTokens = 100000, 80000
	turn, err := factory.NewTurn(ctx, "externalized-loop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := turn.Run(ctx, []*schema.Message{schema.UserMessage("Read the data")}, nil); err != nil {
		t.Fatal(err)
	}
	history, err := factory.LoadHistory(ctx, "externalized-loop")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(history)
	if fake.summaries.Load() != 0 || len(artifacts.items) != 2 || len(encoded) > 12000 || !strings.Contains(string(encoded), "artifact_ref") {
		t.Fatalf("externalized context restored raw output: summaries=%d artifacts=%d history bytes=%d", fake.summaries.Load(), len(artifacts.items), len(encoded))
	}
}

func TestRetentionDoesNotMultiplyRequestCalibration(t *testing.T) {
	counter := tokenizer.NewApproximate("test")
	message := schema.AssistantMessage(strings.Repeat("x", 40), nil)
	messages := make([]*schema.Message, 20)
	for index := range messages {
		messages[index] = message
	}
	for _, adjustment := range []struct{ actual, estimated int }{{1001, 1}, {1, 1001}} {
		counter = tokenizer.NewApproximate("test")
		counter.ObserveProviderInput(adjustment.actual, adjustment.estimated)
		if keep := latestMessageCountWithCounter(counter, messages, 25); keep != 2 {
			t.Fatalf("calibration %+v changed the local retained suffix: %d messages", adjustment, keep)
		}
	}
}

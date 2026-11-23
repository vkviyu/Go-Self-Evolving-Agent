package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenizer"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func TestPrepareMessagesPersistsCompactionBoundary(t *testing.T) {
	store := memorystore.New()
	registry := tool.NewRegistry()
	engine, err := policy.DefaultDeny()
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := tool.NewPipeline(tool.PipelineConfig{Registry: registry, Policy: engine, Events: store})
	if err != nil {
		t.Fatal(err)
	}
	factory, err := NewFactory(Config{ContextWindowTokens: 1000, CompactionTriggerTokens: 15}, &summaryModel{}, registry, pipeline, store, store)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := factory.PrepareMessages(context.Background(), session.ID("compact-session"), []*schema.Message{
		schema.UserMessage(strings.Repeat("old context that should be summarized ", 40)),
		schema.AssistantMessage(strings.Repeat("old answer with decisions ", 40), nil),
		schema.UserMessage("latest request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) < 2 || messages[0].Role != schema.System || messages[len(messages)-1].Content != "latest request" {
		t.Fatalf("prepared messages = %#v", messages)
	}
	history, err := factory.LoadHistory(context.Background(), "compact-session")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].Role != schema.System || history[0].Content != "Conversation summary:\n"+testSemanticSummary {
		t.Fatalf("projected compacted history = %#v", history)
	}
}

func TestContextPolicyExternalizesLargeToolResults(t *testing.T) {
	store := memorystore.New()
	registry := tool.NewRegistry()
	engine, err := policy.DefaultDeny()
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := tool.NewPipeline(tool.PipelineConfig{Registry: registry, Policy: engine, Events: store})
	if err != nil {
		t.Fatal(err)
	}
	artifacts := &memoryContextArtifacts{}
	factory, err := NewFactory(Config{
		ContextWindowTokens: 200000, CompactionTriggerTokens: 150000, ContextArtifacts: artifacts,
	}, &summaryModel{}, registry, pipeline, store, store)
	if err != nil {
		t.Fatal(err)
	}
	large := strings.Repeat("tool-output-", 3000)
	messages, err := factory.PrepareMessages(context.Background(), "artifact-context", []*schema.Message{
		schema.UserMessage("inspect"),
		schema.ToolMessage(large, "call-1", schema.WithToolName("shell_exec")),
		schema.UserMessage("continue"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts.items) != 1 || artifacts.items[0] != large {
		t.Fatalf("artifacts = %d", len(artifacts.items))
	}
	var placeholder struct {
		Reference string `json:"artifact_ref"`
		Bytes     int    `json:"content_bytes"`
	}
	if err := json.Unmarshal([]byte(messages[1].Content), &placeholder); err != nil {
		t.Fatal(err)
	}
	if placeholder.Reference == "" || placeholder.Bytes != len(large) || len(messages[1].Content) >= len(large) {
		t.Fatalf("placeholder = %#v bytes=%d", placeholder, len(messages[1].Content))
	}
	events, err := store.Load(context.Background(), "artifact-context", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != session.EventContextManaged {
		t.Fatalf("management events = %#v", events)
	}
}

func TestContextPolicyAnchorsActiveGoalAcrossCompaction(t *testing.T) {
	store := memorystore.New()
	registry := tool.NewRegistry()
	engine, _ := policy.DefaultDeny()
	pipeline, err := tool.NewPipeline(tool.PipelineConfig{Registry: registry, Policy: engine, Events: store})
	if err != nil {
		t.Fatal(err)
	}
	factory, err := NewFactory(Config{ContextWindowTokens: 4000, CompactionTriggerTokens: 600}, &summaryModel{}, registry, pipeline, store, store)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := factory.PrepareMessagesWithHints(context.Background(), "goal-context", []*schema.Message{
		schema.UserMessage(strings.Repeat("old investigation details ", 200)),
		schema.AssistantMessage(strings.Repeat("old findings ", 200), nil),
		schema.UserMessage("continue"),
	}, ContextHints{ContractID: "plan-1", ContractText: "Fix the durable recovery bug without regressions", Attempt: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared) < 2 || !strings.Contains(prepared[0].Content, "Fix the durable recovery bug without regressions") || !strings.Contains(prepared[0].Content, "attempt=3") {
		t.Fatalf("execution contract anchor missing from prepared context: %#v", prepared)
	}
}

func TestCriticalContextUsesExtractiveFallbackWhenSummaryFails(t *testing.T) {
	store := memorystore.New()
	registry := tool.NewRegistry()
	engine, _ := policy.DefaultDeny()
	pipeline, err := tool.NewPipeline(tool.PipelineConfig{Registry: registry, Policy: engine, Events: store})
	if err != nil {
		t.Fatal(err)
	}
	factory, err := NewFactory(Config{ContextWindowTokens: 16000, CompactionTriggerTokens: 8000}, &failingSummaryModel{}, registry, pipeline, store, store)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := factory.PrepareMessages(context.Background(), "fallback-context", []*schema.Message{
		schema.UserMessage(strings.Repeat("old-context ", 3000)),
		schema.AssistantMessage(strings.Repeat("old-result ", 3000), nil),
		schema.UserMessage("latest request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared) < 2 || !strings.Contains(prepared[0].Content, "Semantic summarization was unavailable") || prepared[len(prepared)-1].Content != "latest request" {
		t.Fatalf("fallback context = %#v", prepared)
	}
}

func TestCompactForcesDurableBoundary(t *testing.T) {
	store := memorystore.New()
	registry := tool.NewRegistry()
	engine, err := policy.DefaultDeny()
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := tool.NewPipeline(tool.PipelineConfig{Registry: registry, Policy: engine, Events: store})
	if err != nil {
		t.Fatal(err)
	}
	factory, err := NewFactory(Config{ContextWindowTokens: 1000, CompactionTriggerTokens: 10}, &summaryModel{}, registry, pipeline, store, store)
	if err != nil {
		t.Fatal(err)
	}
	var live []Event
	prepared, changed, err := factory.CompactWithSink(context.Background(), session.ID("manual-compact"), []*schema.Message{
		schema.UserMessage(strings.Repeat("old question ", 150)),
		schema.AssistantMessage(strings.Repeat("old answer ", 150), nil),
		schema.UserMessage("latest question"),
	}, func(_ context.Context, event Event) error {
		live = append(live, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !changed || len(prepared) != 2 || prepared[0].Role != schema.System {
		t.Fatalf("changed=%v prepared=%#v", changed, prepared)
	}
	var sawCompaction, sawPreparation, sawRequest, sawResponse bool
	for _, event := range live {
		switch event.Type {
		case EventContextCompactionStarted:
			sawCompaction = true
		case EventModelPreparationStarted:
			sawPreparation = sawPreparation || event.Purpose == "compaction"
		case EventModelRequestStarted:
			sawRequest = sawRequest || event.Purpose == "compaction"
		case EventModelResponseStarted:
			sawResponse = sawResponse || event.Purpose == "compaction"
		}
	}
	if !sawCompaction || !sawPreparation || !sawRequest || !sawResponse {
		t.Fatalf("manual compaction live events = %#v", live)
	}
	history, err := factory.LoadHistory(context.Background(), "manual-compact")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[0].Role != schema.System || history[1].Content != "latest question" {
		t.Fatalf("history after manual compaction = %#v", history)
	}
}

func TestCompactionTranscriptReducesLargeRepeatedToolLoop(t *testing.T) {
	largeResult := strings.Repeat("诊断输出", 10000)
	call := func(id string) *schema.Message {
		return &schema.Message{
			Role: schema.Assistant,
			ToolCalls: []schema.ToolCall{{
				ID: id, Type: "function", Function: schema.FunctionCall{Name: "shell_exec", Arguments: `{"command":"go test ./..."}`},
			}},
		}
	}
	transcript, err := compactionTranscript([]*schema.Message{
		call("call-1"), schema.ToolMessage(largeResult, "call-1"),
		call("call-2"), schema.ToolMessage(largeResult, "call-2"),
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(transcript, "\n")
	if !strings.Contains(joined, "compaction omitted") {
		t.Fatalf("large result was not reduced: %d bytes", len(joined))
	}
	if !strings.Contains(joined, "2-message pattern; it occurred 2 times") {
		t.Fatalf("repeated tool loop was not collapsed: %s", joined)
	}
	if strings.Contains(joined, "call-2") {
		t.Fatalf("collapsed retry remained in transcript: %s", joined)
	}
	if len(joined) >= len(largeResult) {
		t.Fatalf("transcript was not reduced: got %d, original result %d", len(joined), len(largeResult))
	}
	if !utf8.ValidString(joined) {
		t.Fatal("reduced transcript is not valid UTF-8")
	}
}

type summaryModel struct{}

const testSemanticSummary = `{"version":1,"source":"semantic","task":"Continue the user's task.","constraints":[],"completed":[],"changes":[],"verification":[],"open_items":[],"next_steps":[],"references":[]}`

type memoryContextArtifacts struct {
	items []string
}

type fixedContextCounter struct{}

func (fixedContextCounter) Model() string                                { return "test" }
func (fixedContextCounter) Encoding() string                             { return "fixed" }
func (fixedContextCounter) Status() tokenizer.Status                     { return tokenizer.StatusExact }
func (fixedContextCounter) Exact() bool                                  { return true }
func (fixedContextCounter) CountMessages(messages []*schema.Message) int { return len(messages) * 30 }
func (fixedContextCounter) ObserveProviderInput(int, int)                {}

func (store *memoryContextArtifacts) Put(_ context.Context, data []byte) (string, error) {
	store.items = append(store.items, string(data))
	return "sha256:test-artifact", nil
}

var _ model.BaseChatModel = (*summaryModel)(nil)

func (*summaryModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return schema.AssistantMessage(testSemanticSummary, nil), nil
}

func (*summaryModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, errors.New("stream not used by compaction")
}

type failingSummaryModel struct{}

func (*failingSummaryModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return nil, errors.New("summary unavailable")
}

func (*failingSummaryModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, errors.New("stream not used by compaction")
}

package agentruntime

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/modelcontext"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenizer"
)

type assessmentModel struct {
	calls, summaries atomic.Int32
	first            *schema.Message
}

func (m *assessmentModel) Generate(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	if len(input) > 0 && strings.HasPrefix(input[0].Content, compactionInstruction) {
		m.summaries.Add(1)
		return schema.AssistantMessage(`{"version":1,"source":"semantic","task":"Continue the conversation","constraints":[],"completed":["Answered greeting"],"changes":[],"verification":[],"open_items":[],"next_steps":[],"references":[]}`, nil), nil
	}
	if m.calls.Add(1) == 1 {
		if m.first != nil {
			return m.first, nil
		}
		return schema.AssistantMessage(strings.Repeat("answer ", 450), nil), nil
	}
	return schema.AssistantMessage("continued", nil), nil
}

func TestTurnAssessmentExcludesTransientReasoningFromReplayBudget(t *testing.T) {
	ctx, store := context.Background(), memorystore.New()
	final := schema.AssistantMessage("done", nil)
	final.ReasoningContent = strings.Repeat("reason ", 6000)
	fake := &assessmentModel{first: final}
	factory := profileFactory(t, store, fake)
	factory.tokenizer = tokenizer.NewApproximate("test")
	factory.config.ContextWindowTokens, factory.config.CompactionTriggerTokens = 4000, 400
	turn, err := factory.NewTurn(ctx, "reasoning")
	if err != nil {
		t.Fatal(err)
	}
	result, err := turn.Run(ctx, []*schema.Message{schema.UserMessage("hello")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	history, err := factory.LoadHistory(ctx, "reasoning")
	if err != nil {
		t.Fatal(err)
	}
	reopened := factory.ContextUsage(history)
	if result.ContextUsage == nil || result.ContextUsage.State != ContextReady || result.ContextUsage.Used != reopened.Used || reopened.NeedsCompaction {
		t.Fatalf("transient reasoning inflated replay context: result=%+v reopened=%+v", result.ContextUsage, reopened)
	}
	if final.ReasoningContent == "" {
		t.Fatal("projection mutated live reasoning")
	}
}

func (m *assessmentModel) Stream(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := m.Generate(ctx, input, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func TestTurnEndMarksPendingWithoutModelCallAndNextDispatchCompacts(t *testing.T) {
	ctx, store, fake := context.Background(), memorystore.New(), &assessmentModel{}
	factory := profileFactory(t, store, fake)
	factory.tokenizer = tokenizer.NewApproximate("test")
	factory.config.ContextWindowTokens, factory.config.CompactionTriggerTokens, factory.config.MaxOutputTokens = 4000, 400, 1000
	turn, err := factory.NewTurn(ctx, "post-turn")
	if err != nil {
		t.Fatal(err)
	}
	result, err := turn.Run(ctx, []*schema.Message{schema.UserMessage("hello")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.ContextUsage == nil || result.ContextUsage.State != ContextCompactionPending || !result.ContextUsage.NeedsCompaction {
		t.Fatalf("missing post-response assessment: %+v", result.ContextUsage)
	}
	if fake.calls.Load() != 1 || fake.summaries.Load() != 0 {
		t.Fatalf("idle assessment called model: calls=%d summaries=%d", fake.calls.Load(), fake.summaries.Load())
	}
	var durable *ContextUsage
	if err := session.VisitEvents(ctx, store, "post-turn", func(event session.Event) error {
		if event.Type == session.EventTurnCompleted {
			var payload struct {
				Assessment *ContextUsage `json:"context_assessment"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return err
			}
			durable = payload.Assessment
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if durable == nil || durable.Used != result.ContextUsage.Used || durable.State != ContextCompactionPending {
		t.Fatalf("assessment not atomic with completion: %+v", durable)
	}
	history, err := factory.LoadHistory(ctx, "post-turn")
	if err != nil {
		t.Fatal(err)
	}
	usage := factory.ContextUsage(history)
	if usage.State != ContextCompactionPending || usage.Used != durable.Used {
		t.Fatalf("reopened history disagrees with assessment: %+v durable=%+v", usage, durable)
	}
	next, err := factory.NewTurn(ctx, "post-turn")
	if err != nil {
		t.Fatal(err)
	}
	_, err = next.Run(ctx, append(history, schema.UserMessage("continue")), nil)
	if err != nil {
		t.Fatal(err)
	}
	if fake.calls.Load() != 2 || fake.summaries.Load() != 1 {
		t.Fatalf("next dispatch must compact once: calls=%d summaries=%d", fake.calls.Load(), fake.summaries.Load())
	}
}

func TestCompletedAssessmentCountsPreparedInputAndToolsOnce(t *testing.T) {
	store := memorystore.New()
	factory := profileFactory(t, store, &assessmentModel{})
	factory.tokenizer = tokenizer.NewApproximate("test")
	factory.config.ContextWindowTokens, factory.config.CompactionTriggerTokens = 1000, 600
	m := &recordingModel{factory: factory}
	input := []*schema.Message{schema.SystemMessage("host instruction"), schema.UserMessage("hello")}
	options := []model.Option{model.WithTools([]*schema.ToolInfo{{Name: "read", Desc: "read a file"}}), model.WithMaxTokens(100)}
	if err := m.captureRequest(input, options); err != nil {
		t.Fatal(err)
	}
	final := schema.AssistantMessage(strings.Repeat("answer ", 600), nil)
	want := factory.countContextInput(append(append([]*schema.Message(nil), input...), final), requestToolSchemas(options))
	// Eino may annotate or reuse the caller's messages after dispatch.
	input[0].Content = strings.Repeat("unexpected mutation ", 200)
	usage := m.completedContext(final)
	if usage.Used != want || usage.State != ContextOverBudget || !usage.NeedsCompaction {
		t.Fatalf("prepared request counted incorrectly: %+v want=%d", usage, want)
	}
}

type capacityModel struct {
	assessmentModel
	metadata modelcontext.Metadata
}

func (m *capacityModel) ModelContext() modelcontext.Metadata { return m.metadata }

func TestFactoryEnforcesProviderWindowWithExplicitOverridesAndOutputCap(t *testing.T) {
	store := memorystore.New()
	fake := &capacityModel{metadata: modelcontext.Metadata{Window: 8192, Source: modelcontext.SourceProvider, MaxOutputTokens: 1024, OutputSource: modelcontext.SourceProvider}}
	base := profileFactory(t, store, fake)
	if _, err := NewFactory(Config{ContextWindowTokens: 16000}, fake, base.registry, base.pipeline, store, store); err == nil {
		t.Fatal("explicit override bypassed provider hard limit")
	}
	factory, err := NewFactory(Config{ContextWindowTokens: 4096, MaxOutputTokens: 3000}, fake, base.registry, base.pipeline, store, store)
	if err != nil {
		t.Fatal(err)
	}
	usage := factory.ContextUsage(nil)
	if usage.OutputReserve != 1024 || usage.Window != 4096 || usage.Capacity.Window != 8192 || usage.WindowSource != modelcontext.SourceConfigured {
		t.Fatalf("policy and capacity not independent: %+v", usage)
	}
	if usage.InputLimit != 4096-1024-modelcontext.SafetyMargin(4096) {
		t.Fatalf("reserves subtracted incorrectly: %+v", usage)
	}
}

func TestDifferentProfileModelResolvesAutomaticOutputAgainstRecipient(t *testing.T) {
	ctx, store := context.Background(), memorystore.New()
	base := profileFactory(t, store, &assessmentModel{})
	parent, err := NewFactory(Config{ModelProfile: "primary"}, base.model, base.registry, base.pipeline, store, store)
	if err != nil {
		t.Fatal(err)
	}
	parent.config.ResolveModel = func(context.Context, string) (ModelBinding, error) {
		return ModelBinding{Model: &capacityModel{metadata: modelcontext.Metadata{Window: 1000, Source: modelcontext.SourceProvider}}, Tokenizer: fixedContextCounter{}, Provider: "test", Name: "small", ContextWindowTokens: 1000}, nil
	}
	for _, profileLimit := range []int{0, 17, 10000} {
		profile := runtimeTestProfile()
		profile.ModelProfile, profile.Limits.MaxOutputTokens = "small", profileLimit
		child, err := parent.WithProfile(ctx, runtimeTestScope(t), "child-auto", profile)
		if err != nil {
			t.Fatalf("limit=%d: %v", profileLimit, err)
		}
		want := 250
		if profileLimit > 0 {
			want = min(want, profileLimit)
		}
		if child.config.MaxOutputTokens != want || parent.config.MaxOutputTokens != 8192 {
			t.Fatalf("limit=%d inherited resolved parent allowance: parent=%d child=%d", profileLimit, parent.config.MaxOutputTokens, child.config.MaxOutputTokens)
		}
	}
}

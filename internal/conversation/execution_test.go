package conversation

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

type executionModel struct{ calls atomic.Int32 }

func (fake *executionModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	fake.calls.Add(1)
	return schema.AssistantMessage("answer", nil), nil
}

type failOnceExecutionModel struct{ calls atomic.Int32 }

func (fake *failOnceExecutionModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	if fake.calls.Add(1) == 1 {
		return nil, timeoutExecutionError{}
	}
	return schema.AssistantMessage("recovered answer", nil), nil
}

func (fake *failOnceExecutionModel) Stream(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := fake.Generate(ctx, input, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

type timeoutExecutionError struct{}

func (timeoutExecutionError) Error() string   { return "upstream timeout" }
func (timeoutExecutionError) Timeout() bool   { return true }
func (timeoutExecutionError) Temporary() bool { return true }

func (fake *executionModel) Stream(ctx context.Context, messages []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := fake.Generate(ctx, messages, options...)
	return schema.StreamReaderFromArray([]*schema.Message{message}), err
}

type historyFailureStore struct {
	*memorystore.Store
	failLoad           atomic.Bool
	failAfterCompleted bool
	failure            error
}

func (store *historyFailureStore) Load(ctx context.Context, id session.ID, after uint64, limit int) ([]session.Event, error) {
	if store.failLoad.Load() {
		return nil, store.failure
	}
	return store.Store.Load(ctx, id, after, limit)
}

func (store *historyFailureStore) Append(ctx context.Context, id session.ID, expected uint64, pending ...session.PendingEvent) ([]session.Event, error) {
	events, err := store.Store.Append(ctx, id, expected, pending...)
	if err == nil && store.failAfterCompleted {
		for _, event := range events {
			if event.Type == session.EventTurnCompleted {
				store.failLoad.Store(true)
			}
		}
	}
	return events, err
}

func executionFixture(t *testing.T, store *historyFailureStore) (*Conversation, *executionModel) {
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
	fake := &executionModel{}
	factory, err := agentruntime.NewFactory(agentruntime.Config{}, fake, registry, pipeline, store, store)
	if err != nil {
		t.Fatal(err)
	}
	return &Conversation{factory: factory, sessionID: "execution"}, fake
}

func TestContextProjectionFailureStopsBeforeTurnCreation(t *testing.T) {
	failure := errors.New("projection unavailable")
	store := &historyFailureStore{Store: memorystore.New(), failure: failure}
	store.failLoad.Store(true)
	chat, fake := executionFixture(t, store)
	chat.history = []*schema.Message{schema.AssistantMessage("old projection", nil)}
	_, err := chat.RunTurn(context.Background(), "new input", nil)
	if !errors.Is(err, failure) || !strings.Contains(err.Error(), "refresh history after context preparation") {
		t.Fatalf("error=%v", err)
	}
	if fake.calls.Load() != 0 || chat.CompletedTurns() != 0 || chat.History()[0].Content != "old projection" {
		t.Fatal("projection failure mutated history or dispatched work")
	}
	if sequence, _ := store.CurrentSequence(context.Background(), chat.ID()); sequence != 0 {
		t.Fatal("turn was created before projection refresh")
	}
}

func TestInnerTurnAdmissionCallbackPrecedesExecution(t *testing.T) {
	store := &historyFailureStore{Store: memorystore.New()}
	chat, fake := executionFixture(t, store)
	failure := errors.New("round reservation failed")
	var bound string
	chat.mutex.Lock()
	_, err := chat.runInnerTurn(context.Background(), "input", agentruntime.ContextHints{}, nil, func(id string) error {
		bound = id
		if fake.calls.Load() != 0 {
			t.Error("worker executed before round reservation")
		}
		return failure
	})
	chat.mutex.Unlock()
	if !errors.Is(err, failure) || bound == "" || fake.calls.Load() != 0 {
		t.Fatalf("bound=%s calls=%d err=%v", bound, fake.calls.Load(), err)
	}
	events, err := store.Load(context.Background(), chat.ID(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	seenStart, seenInput := false, false
	for _, event := range events {
		seenStart = seenStart || event.Type == session.EventTurnStarted
		seenInput = seenInput || event.Type == session.EventUserMessage
	}
	if !seenStart || !seenInput {
		t.Fatal("turn input was not durable before rejected admission")
	}
}

func TestFreshSessionSkipsHistoryLoadingPhase(t *testing.T) {
	store := &historyFailureStore{Store: memorystore.New()}
	chat, _ := executionFixture(t, store)
	var events []agentruntime.EventType
	_, err := chat.RunTurn(context.Background(), "first input", func(_ context.Context, event agentruntime.Event) error {
		events = append(events, event.Type)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event == agentruntime.EventConversationHistoryLoading {
			t.Fatal("fresh session emitted conversation history loading")
		}
	}
	// Construction happens before RunWithBefore installs its own live sink.
	// Verify the public conversation path exposes those storage waits too.
	position := 0
	for _, expected := range []agentruntime.EventType{
		agentruntime.EventTurnSnapshotRetentionStarted,
		agentruntime.EventTurnSessionSequenceReadStarted,
		agentruntime.EventTurnSessionCreationStarted,
		agentruntime.EventTurnAgentConstructionStarted,
		agentruntime.EventTurnPersistenceStarted,
		agentruntime.EventModelRequestStarted,
	} {
		for position < len(events) && events[position] != expected {
			position++
		}
		if position == len(events) {
			t.Fatalf("missing ordered phase %s: %v", expected, events)
		}
		position++
	}
}

func TestPostExecutionProjectionFailureKeepsDurableOutcome(t *testing.T) {
	failure := errors.New("projection unavailable")
	store := &historyFailureStore{Store: memorystore.New(), failure: failure, failAfterCompleted: true}
	chat, fake := executionFixture(t, store)
	_, err := chat.RunTurn(context.Background(), "input", nil)
	if !errors.Is(err, failure) || !strings.Contains(err.Error(), "refresh session history:") {
		t.Fatalf("error=%v", err)
	}
	if fake.calls.Load() != 1 || chat.CompletedTurns() != 0 || len(chat.History()) != 0 {
		t.Fatal("failed projection published an unrefreshed view")
	}
	store.failLoad.Store(false)
	reopened, err := Open(context.Background(), chat.factory, chat.ID())
	if err != nil {
		t.Fatal(err)
	}
	if reopened.CompletedTurns() != 1 || len(reopened.History()) != 2 || fake.calls.Load() != 1 {
		t.Fatal("durable completed outcome lost or re-executed")
	}
}

func TestTurnJournalResumesAfterModelFailureWithoutCheckpoint(t *testing.T) {
	store := &historyFailureStore{Store: memorystore.New()}
	registry := tool.NewRegistry()
	engine, err := policy.DefaultDeny()
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := tool.NewPipeline(tool.PipelineConfig{Registry: registry, Policy: engine, Events: store})
	if err != nil {
		t.Fatal(err)
	}
	fake := &failOnceExecutionModel{}
	factory, err := agentruntime.NewFactory(agentruntime.Config{}, fake, registry, pipeline, store, store)
	if err != nil {
		t.Fatal(err)
	}
	chat := &Conversation{factory: factory, sessionID: "journal-recovery"}
	if _, err := chat.RunTurn(context.Background(), "resume after outage", nil); err == nil {
		t.Fatal("first model request unexpectedly succeeded")
	}
	recoveries, err := chat.Recoveries(context.Background())
	if err != nil || len(recoveries) != 1 || recoveries[0].Phase != "execution" {
		t.Fatalf("journal recovery was not advertised: %#v %v", recoveries, err)
	}
	turns, err := factory.TurnRecords(context.Background(), chat.ID())
	if err != nil || len(turns) != 1 || turns[0].HasCheckpoint || !turns[0].HasJournal {
		t.Fatalf("unexpected journal/checkpoint state: %#v %v", turns, err)
	}
	result, err := chat.ResumeTurn(context.Background(), recoveries[0].TurnID, nil)
	if err != nil || result.Final == nil || result.Final.Content != "recovered answer" {
		t.Fatalf("journal resume failed: %#v %v", result, err)
	}
	if fake.calls.Load() != 2 {
		t.Fatalf("resume did not issue exactly one replacement model request: %d", fake.calls.Load())
	}
}

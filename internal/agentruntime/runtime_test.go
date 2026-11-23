package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/approval"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

type blockedSequenceStore struct {
	session.EventStore
	entered chan Event
	release chan struct{}
	latest  Event
	failure error
}

func (s *blockedSequenceStore) CurrentSequence(ctx context.Context, _ session.ID) (uint64, error) {
	s.entered <- s.latest
	select {
	case <-s.release:
		return 0, s.failure
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func TestNewTurnEmitsSessionSequenceStageBeforeBlockedRead(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	store := memorystore.New()
	failure := errors.New("session sequence unavailable")
	blocked := &blockedSequenceStore{
		EventStore: store, entered: make(chan Event, 1), release: make(chan struct{}), failure: failure,
	}
	factory := profileFactory(t, store, lifecycleModel{})
	factory.events = blocked
	var live []Event
	ctx = WithLiveSink(ctx, func(_ context.Context, event Event) error {
		blocked.latest = event
		live = append(live, event)
		return errors.New("presentation unavailable")
	})
	done := make(chan error, 1)
	go func() {
		_, err := factory.NewTurn(ctx, "blocked-session")
		done <- err
	}()
	select {
	case event := <-blocked.entered:
		if event.Type != EventTurnSessionSequenceReadStarted || event.TurnID == "" {
			t.Fatalf("stage at blocked CurrentSequence = %#v", event)
		}
	case err := <-done:
		t.Fatalf("NewTurn returned before the sequence read: %v", err)
	}
	close(blocked.release)
	if err := <-done; !errors.Is(err, failure) {
		t.Fatalf("NewTurn error = %v, want underlying store failure", err)
	}
	if len(live) != 2 || live[0].Type != EventTurnSnapshotRetentionStarted || live[1].Type != EventTurnSessionSequenceReadStarted {
		t.Fatalf("unexpected stages for a failed turn without strategy/mailbox: %#v", live)
	}
	if current, err := store.CurrentSequence(ctx, "blocked-session"); err != nil || current != 0 {
		t.Fatalf("live stages changed durable history: sequence=%d err=%v", current, err)
	}
}

func TestEinoAgentLoopUsesPolicyPipelineAndDurableEvents(t *testing.T) {
	t.Parallel()

	events := memorystore.New()
	registry := tool.NewRegistry()
	if _, err := registry.Activate(tool.Binding{
		Descriptor: tool.Descriptor{
			Name:        "echo",
			Description: "Echoes a value for the Eino loop test.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"]}`),
			Revision:    "test-v1",
			RiskClass:   tool.RiskPure,
		},
		Executor: tool.InvokeFunc(func(_ context.Context, arguments json.RawMessage) (json.RawMessage, error) {
			return arguments, nil
		}),
	}); err != nil {
		t.Fatal(err)
	}
	policyEngine, err := policy.DefaultDeny(policy.Rule{
		ID:                "allow-test-echo",
		ToolPattern:       "echo",
		CapabilityPattern: "tool.invoke",
		Effect:            policy.EffectAllow,
	})
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := tool.NewPipeline(tool.PipelineConfig{Registry: registry, Policy: policyEngine, Events: events})
	if err != nil {
		t.Fatal(err)
	}
	factory, err := NewFactory(Config{Instruction: "Use the echo tool once."}, &scriptedModel{}, registry, pipeline, events, events)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := factory.NewTurn(context.Background(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	result, err := turn.Run(context.Background(), []*schema.Message{schema.UserMessage("echo hello")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Final == nil || result.Final.Content != "finished after tool" {
		t.Fatalf("unexpected final message: %#v", result.Final)
	}

	recorded, err := events.Load(context.Background(), "session-1", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	wantTypes := []session.EventType{
		session.EventSessionCreated,
		session.EventTurnStarted,
		session.EventUserMessage,
		session.EventTurnPrepared,
		session.EventModelRequested,
		session.EventModelPartial,
		session.EventAssistantMessage,
		session.EventToolProposed,
		session.EventPolicyDecided,
		session.EventToolStarted,
		session.EventToolSucceeded,
		session.EventTurnPrepared,
		session.EventModelRequested,
		session.EventModelPartial,
		session.EventAssistantMessage,
		session.EventTurnCompleted,
	}
	if len(recorded) != len(wantTypes) {
		for _, event := range recorded {
			t.Log(event.Type)
		}
		t.Fatalf("got %d events, want %d", len(recorded), len(wantTypes))
	}
	for index, want := range wantTypes {
		if recorded[index].Type != want {
			t.Fatalf("event %d = %q, want %q", index, recorded[index].Type, want)
		}
	}
	if recorded[7].CorrelationID != "model-call-1" {
		t.Fatalf("tool correlation ID = %q, want model-call-1", recorded[7].CorrelationID)
	}
	history, err := factory.LoadHistory(context.Background(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 4 || history[0].Role != schema.User || history[1].Role != schema.Assistant || len(history[1].ToolCalls) != 1 || history[2].Role != schema.Tool || history[3].Content != "finished after tool" {
		t.Fatalf("unexpected projected history: %#v", history)
	}
	if history[3].ResponseMeta == nil || history[3].ResponseMeta.Usage == nil || history[3].ResponseMeta.Usage.PromptTokenDetails.CachedTokens != 12 || history[3].ResponseMeta.Usage.CompletionTokensDetails.ReasoningTokens != 2 {
		t.Fatalf("cached token usage was not restored: %#v", history[3].ResponseMeta)
	}
}

func TestResumeRestoresDurableModelRequestSequence(t *testing.T) {
	events := memorystore.New()
	if _, err := events.Append(context.Background(), "sequence-session", session.AnySequence,
		session.PendingEvent{Type: session.EventModelRequested, CorrelationID: "turn-1", Payload: session.MustJSONPayload(map[string]any{
			"request_id": "turn-1:3", "request_sequence": 3,
		})},
		session.PendingEvent{Type: session.EventModelRequested, CorrelationID: "turn-1", Payload: session.MustJSONPayload(map[string]any{
			"request_id": "turn-1:7",
		})},
	); err != nil {
		t.Fatal(err)
	}
	factory := &Factory{events: events}
	sequence, err := factory.modelRequestSequence(context.Background(), "sequence-session", "turn-1")
	if err != nil {
		t.Fatal(err)
	}
	if sequence != 7 {
		t.Fatalf("restored request sequence = %d, want 7", sequence)
	}
}

func TestResumeRejectsModelSnapshotDrift(t *testing.T) {
	events := memorystore.New()
	if _, err := events.Append(context.Background(), "model-drift-session", session.AnySequence,
		session.PendingEvent{Type: session.EventTurnStarted, CorrelationID: "turn-1", Payload: session.MustJSONPayload(map[string]any{
			"turn_id": "turn-1", "provider": "openai", "model": "model-a",
		})},
	); err != nil {
		t.Fatal(err)
	}
	factory := &Factory{events: events, config: Config{Provider: "openai", Model: "model-b"}}
	if err := factory.validateTurnModel(context.Background(), "model-drift-session", "turn-1"); !errors.Is(err, ErrTurnModelChanged) {
		t.Fatalf("model drift error = %v, want ErrTurnModelChanged", err)
	}
}

func TestDeniedApprovalReturnsToolResultAndContinuesTurn(t *testing.T) {
	events := memorystore.New()
	registry := tool.NewRegistry()
	if _, err := registry.Activate(tool.Binding{
		Descriptor: tool.Descriptor{
			Name:        "echo",
			Description: "Echoes a value for the approval test.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}}}`),
			Revision:    "test-v1",
			RiskClass:   tool.RiskPure,
		},
		Executor: tool.InvokeFunc(func(_ context.Context, arguments json.RawMessage) (json.RawMessage, error) {
			return arguments, nil
		}),
	}); err != nil {
		t.Fatal(err)
	}
	policyEngine, err := policy.DefaultDeny(policy.Rule{
		ID:                "require-test-approval",
		ToolPattern:       "echo",
		CapabilityPattern: "tool.invoke",
		Effect:            policy.EffectRequireApproval,
	})
	if err != nil {
		t.Fatal(err)
	}
	broker := approval.NewBroker(1)
	t.Cleanup(func() { _ = broker.Close() })
	pipeline, err := tool.NewPipeline(tool.PipelineConfig{
		Registry: registry,
		Policy:   policyEngine,
		Approver: broker,
		Events:   events,
	})
	if err != nil {
		t.Fatal(err)
	}
	factory, err := NewFactory(Config{Instruction: "Use the echo tool and report the result."}, &deniedApprovalModel{}, registry, pipeline, events, events)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := factory.NewTurn(context.Background(), "session-approval")
	if err != nil {
		t.Fatal(err)
	}
	resultCh := make(chan struct {
		result Result
		err    error
	}, 1)
	go func() {
		result, runErr := turn.Run(context.Background(), []*schema.Message{schema.UserMessage("run echo")}, nil)
		resultCh <- struct {
			result Result
			err    error
		}{result: result, err: runErr}
	}()
	request := <-broker.Requests()
	if err := broker.Resolve(request.ID, false); err != nil {
		t.Fatal(err)
	}
	completed := <-resultCh
	if completed.err != nil {
		t.Fatalf("denied approval failed turn: %v", completed.err)
	}
	if completed.result.Final == nil || completed.result.Final.Content != "finished after denied tool" {
		t.Fatalf("unexpected final message: %#v", completed.result.Final)
	}
	history, err := factory.LoadHistory(context.Background(), "session-approval")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 4 || history[2].Role != schema.Tool || !strings.Contains(history[2].Content, "approval denied") {
		t.Fatalf("denied tool result missing from projected history: %#v", history)
	}
}

type deniedApprovalModel struct{ calls int }

var _ model.BaseChatModel = (*deniedApprovalModel)(nil)

func (m *deniedApprovalModel) Generate(_ context.Context, input []*schema.Message, options ...model.Option) (*schema.Message, error) {
	m.calls++
	common := model.GetCommonOptions(nil, options...)
	if len(common.Tools) != 1 {
		return nil, errors.New("approval test tool was not bound")
	}
	switch m.calls {
	case 1:
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID:   "approval-call-1",
			Type: "function",
			Function: schema.FunctionCall{
				Name:      "echo",
				Arguments: `{"value":"hello"}`,
			},
		}}), nil
	case 2:
		if len(input) == 0 || input[len(input)-1].Role != schema.Tool || !strings.Contains(input[len(input)-1].Content, "approval denied") {
			return nil, errors.New("denied tool result was not returned to the model")
		}
		return schema.AssistantMessage("finished after denied tool", nil), nil
	default:
		return nil, errors.New("unexpected extra approval test iteration")
	}
}

func (m *deniedApprovalModel) Stream(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := m.Generate(ctx, input, options...)
	if err != nil {
		return nil, err
	}
	reader, writer := schema.Pipe[*schema.Message](1)
	go func() {
		defer writer.Close()
		writer.Send(message, nil)
	}()
	return reader, nil
}

type scriptedModel struct {
	mu    sync.Mutex
	calls int
}

var _ model.BaseChatModel = (*scriptedModel)(nil)

func (m *scriptedModel) Generate(_ context.Context, input []*schema.Message, options ...model.Option) (*schema.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	common := model.GetCommonOptions(nil, options...)
	if len(common.Tools) != 1 || common.Tools[0].Name != "echo" {
		return nil, errors.New("Eino did not bind the registry tool metadata")
	}
	switch m.calls {
	case 1:
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID:   "model-call-1",
			Type: "function",
			Function: schema.FunctionCall{
				Name:      "echo",
				Arguments: `{"value":"hello"}`,
			},
		}}), nil
	case 2:
		if len(input) == 0 || input[len(input)-1].Role != schema.Tool {
			return nil, errors.New("tool result was not returned to the model")
		}
		message := schema.AssistantMessage("finished after tool", nil)
		message.ResponseMeta = &schema.ResponseMeta{Usage: &schema.TokenUsage{
			PromptTokens: 20,
			PromptTokenDetails: schema.PromptTokenDetails{
				CachedTokens: 12,
			},
			CompletionTokens: 4,
			TotalTokens:      24,
			CompletionTokensDetails: schema.CompletionTokensDetails{
				ReasoningTokens: 2,
			},
		}}
		return message, nil
	default:
		return nil, errors.New("unexpected extra model iteration")
	}
}

func (m *scriptedModel) Stream(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := m.Generate(ctx, input, options...)
	if err != nil {
		return nil, err
	}
	reader, writer := schema.Pipe[*schema.Message](1)
	go func() {
		defer writer.Close()
		writer.Send(message, nil)
	}()
	return reader, nil
}

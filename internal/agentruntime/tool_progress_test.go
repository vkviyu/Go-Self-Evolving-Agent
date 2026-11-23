package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func TestEinoTurnStopsRepeatedToolCycleAndAllowsProgress(t *testing.T) {
	for _, test := range []struct {
		name         string
		pages        []int
		changeResult bool
		wantCalls    int
		wantStop     bool
	}{
		{name: "same batch", pages: []int{1, 1, 1, 1, 2}, wantCalls: 4, wantStop: true},
		{name: "alternating batches", pages: []int{1, 2, 1, 2, 1, 2, 1, 2, 3}, wantCalls: 8, wantStop: true},
		{name: "advancing pages", pages: []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}, wantCalls: 13},
		{name: "new result", pages: []int{1, 1, 1, 1, 1, 1}, changeResult: true, wantCalls: 7},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			events := memorystore.New()
			registry := tool.NewRegistry()
			invocations := 0
			if _, err := registry.Activate(tool.Binding{
				Descriptor: tool.Descriptor{
					Name: "fixture_read", Revision: "fixture-v1", Description: "Read a page of evidence.", RiskClass: tool.RiskRead,
					InputSchema: json.RawMessage(`{"type":"object","properties":{"page":{"type":"integer"}},"required":["page"]}`),
				},
				Executor: tool.InvokeFunc(func(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
					invocations++
					if test.changeResult {
						return json.RawMessage(fmt.Sprintf(`{"revision":%d}`, invocations)), nil
					}
					return args, nil
				}),
			}); err != nil {
				t.Fatal(err)
			}
			permissions, err := policy.DefaultDeny(policy.Rule{ID: "read", ToolPattern: "fixture_read", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow})
			if err != nil {
				t.Fatal(err)
			}
			pipeline, err := tool.NewPipeline(tool.PipelineConfig{Registry: registry, Policy: permissions, Events: events})
			if err != nil {
				t.Fatal(err)
			}
			fixture := &pageModel{pages: test.pages}
			factory, err := NewFactory(Config{}, fixture, registry, pipeline, events, events)
			if err != nil {
				t.Fatal(err)
			}
			turn, err := factory.NewTurn(ctx, "tool-progress")
			if err != nil {
				t.Fatal(err)
			}
			result, err := turn.Run(ctx, []*schema.Message{schema.UserMessage("Read the report")}, nil)
			if test.wantStop {
				if !errors.Is(err, ErrRepeatedToolCycle) || invocations != test.wantCalls {
					t.Fatalf("expected repeat stop after %d executions, got %d: %v", test.wantCalls, invocations, err)
				}
				if _, err := factory.ResumeTurn(ctx, "tool-progress", turn.ID()); !errors.Is(err, session.ErrRecoveryToolCycle) {
					t.Fatalf("stopped cycle can still be resumed: %v", err)
				}
			} else if err != nil || result.Final == nil || result.Final.Content != "read complete" || invocations != len(test.pages) {
				t.Fatalf("progress unexpectedly interrupted, invocations=%d result=%+v err=%v", invocations, result, err)
			}
			if fixture.calls != test.wantCalls {
				t.Fatalf("model dispatched %d times, want %d", fixture.calls, test.wantCalls)
			}
		})
	}
}

type pageModel struct {
	pages []int
	calls int
}

func (m *pageModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	index := m.calls
	m.calls++
	if index >= len(m.pages) {
		return schema.AssistantMessage("read complete", nil), nil
	}
	return schema.AssistantMessage("", []schema.ToolCall{{
		ID: fmt.Sprintf("read-%d", index), Type: "function",
		Function: schema.FunctionCall{Name: "fixture_read", Arguments: fmt.Sprintf(`{"page":%d}`, m.pages[index])},
	}}), nil
}

func (m *pageModel) Stream(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := m.Generate(ctx, input, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

package tool

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
)

type (
	durabilityBudgetStore struct {
		session.EventStore
		check func(context.Context, session.EventType)
		after func(session.EventType)
	}
	durabilityValueKey struct{}
)

// Append observes the actual context crossing the durable outcome boundary.
func (s *durabilityBudgetStore) Append(ctx context.Context, id session.ID, sequence uint64, pending ...session.PendingEvent) ([]session.Event, error) {
	for _, event := range pending {
		if s.check != nil {
			s.check(ctx, event.Type)
		}
	}
	events, err := s.EventStore.Append(ctx, id, sequence, pending...)
	if err == nil && s.after != nil {
		for _, event := range events {
			s.after(event.Type)
		}
	}
	return events, err
}

func TestReconciledStartDoesNotExecuteAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	registry := NewRegistry()
	binding := testBinding("rev-1", "ok")
	binding.Descriptor.RiskClass = RiskWrite
	executions := 0
	binding.Executor = InvokeFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
		executions++
		return json.RawMessage(`{"ok":true}`), nil
	})
	if _, err := registry.Activate(binding); err != nil {
		t.Fatal(err)
	}
	store := &durabilityBudgetStore{EventStore: memorystore.New(), after: func(kind session.EventType) {
		if kind == session.EventToolStarted {
			cancel()
		}
	}}
	pipeline := mustPipeline(t, registry, &recordingPolicyEngine{}, nil, store)
	call := Call{SessionID: "cancel-after-start", TurnID: "turn", CallID: "call", ToolName: "echo", Arguments: json.RawMessage(`{}`)}
	if _, err := pipeline.Execute(ctx, call); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if executions != 0 {
		t.Fatal("tool executed after a cancelled start")
	}
	events, err := store.Load(t.Context(), call.SessionID, 0, 100)
	if err != nil || len(events) == 0 || events[len(events)-1].Type != session.EventToolFailed {
		t.Fatalf("nonexecution was not recorded: %v %+v", err, events)
	}
}

func TestToolOutcomeUsesHostBudgetAfterCancellation(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[failed], func(t *testing.T) {
			parent := context.WithValue(context.Background(), durabilityValueKey{}, "trusted-invocation")
			ctx, cancel := context.WithCancel(parent)
			defer cancel()
			registry := NewRegistry()
			binding := testBinding("rev-1", "ok")
			binding.Descriptor.RiskClass = RiskWrite
			executions := 0
			binding.Executor = InvokeFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
				executions++
				cancel()
				if failed {
					return nil, errors.New("reported tool failure")
				}
				return json.RawMessage(`{"ok":true}`), nil
			})
			if _, err := registry.Activate(binding); err != nil {
				t.Fatal(err)
			}
			const budget = 45 * time.Second
			observed := false
			store := &durabilityBudgetStore{EventStore: memorystore.New()}
			store.check = func(writeContext context.Context, kind session.EventType) {
				if kind != session.EventToolSucceeded && kind != session.EventToolFailed {
					return
				}
				observed = true
				if ctx.Err() == nil || writeContext.Err() != nil || writeContext.Value(durabilityValueKey{}) != "trusted-invocation" {
					t.Fatal("outcome lost cancellation isolation or trusted values")
				}
				deadline, ok := writeContext.Deadline()
				remaining := time.Until(deadline)
				if !ok || remaining < budget-time.Second || remaining > budget {
					t.Fatalf("host budget was replaced: %v", remaining)
				}
			}
			pipeline, err := NewPipeline(PipelineConfig{
				Registry: registry, Policy: &recordingPolicyEngine{}, Events: store,
				DurabilityTimeout: budget,
			})
			if err != nil {
				t.Fatal(err)
			}
			pipeline = pipeline.WithApprover(nil)
			call := Call{SessionID: "budget", TurnID: "turn", CallID: "call", ToolName: "echo", Arguments: json.RawMessage(`{}`)}
			_, err = pipeline.Execute(ctx, call)
			var execution *ExecutionError
			if (!failed && err != nil) || (failed && !errors.As(err, &execution)) {
				t.Fatalf("unexpected outcome: %v", err)
			}
			if !observed || executions != 1 {
				t.Fatalf("observed=%v executions=%d", observed, executions)
			}
			// A fresh pipeline reads the saved outcome without rerunning the tool.
			pipeline = mustPipeline(t, registry, &recordingPolicyEngine{}, nil, store)
			_, _ = pipeline.Execute(context.Background(), call)
			if executions != 1 {
				t.Fatal("durable outcome replayed the executor")
			}
		})
	}
}

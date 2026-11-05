package tool

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
)

type failingOutcomeStore struct {
	session.EventStore
	fail session.EventType
}

func (store *failingOutcomeStore) Append(ctx context.Context, id session.ID, sequence uint64, pending ...session.PendingEvent) ([]session.Event, error) {
	for _, event := range pending {
		if event.Type == store.fail {
			return nil, errors.New("outcome storage unavailable")
		}
	}
	return store.EventStore.Append(ctx, id, sequence, pending...)
}

func TestUncertainOutcomeNeverReplaysSideEffects(t *testing.T) {
	for _, risk := range []RiskClass{RiskPure, RiskRead, RiskWrite, RiskExecute, RiskSecret} {
		t.Run(string(risk), func(t *testing.T) {
			registry := NewRegistry()
			invocations := 0
			binding := testBinding("rev-1", "ok")
			binding.Descriptor.RiskClass = risk
			binding.Executor = InvokeFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
				invocations++
				return json.RawMessage(`{"ok":true}`), nil
			})
			if _, err := registry.Activate(binding); err != nil {
				t.Fatal(err)
			}
			engine, err := policy.DefaultDeny(policy.Rule{ID: "allow", ToolPattern: "echo", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow})
			if err != nil {
				t.Fatal(err)
			}
			store := &failingOutcomeStore{EventStore: memorystore.New(), fail: session.EventToolSucceeded}
			pipeline := mustPipeline(t, registry, engine, nil, store)
			call := Call{SessionID: "uncertain", TurnID: "turn", CallID: "call", ToolName: "echo", Arguments: json.RawMessage(`{}`)}
			_, err = pipeline.Execute(context.Background(), call)
			var unknown *OutcomeUnknownError
			if !errors.As(err, &unknown) || invocations != 1 {
				t.Fatalf("first call err=%v executions=%d", err, invocations)
			}
			store.fail = ""
			// A fresh pipeline models restart, sharing only durable events.
			pipeline = mustPipeline(t, registry, engine, nil, store)
			_, err = pipeline.Execute(context.Background(), call)
			if risk == RiskRead || risk == RiskPure {
				if err != nil || invocations != 2 {
					t.Fatalf("read-only retry err=%v executions=%d", err, invocations)
				}
			} else {
				if !errors.As(err, &unknown) || invocations != 1 {
					t.Fatalf("unsafe retry err=%v executions=%d", err, invocations)
				}
			}
		})
	}
}

func TestSuccessPersistsAfterCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	registry := NewRegistry()
	binding := testBinding("rev-1", "ok")
	binding.Descriptor.RiskClass = RiskWrite
	invocations := 0
	binding.Executor = InvokeFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
		invocations++
		cancel() // Side effect completed just as the user stopped the turn.
		return json.RawMessage(`{"ok":true}`), nil
	})
	if _, err := registry.Activate(binding); err != nil {
		t.Fatal(err)
	}
	pipeline := mustPipeline(t, registry, &recordingPolicyEngine{}, nil, memorystore.New())
	call := Call{SessionID: "cancel", TurnID: "turn", CallID: "call", ToolName: "echo", Arguments: json.RawMessage(`{}`)}
	if _, err := pipeline.Execute(ctx, call); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.Execute(context.Background(), call); err != nil {
		t.Fatal(err)
	}
	if invocations != 1 {
		t.Fatalf("canceled success replayed %d times", invocations)
	}
}

func TestFailurePersistenceErrorIsNotRecoverableToolError(t *testing.T) {
	registry := NewRegistry()
	binding := testBinding("rev-1", "ok")
	binding.Descriptor.RiskClass = RiskWrite
	binding.Executor = InvokeFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return nil, errors.New("partial write failed")
	})
	if _, err := registry.Activate(binding); err != nil {
		t.Fatal(err)
	}
	store := &failingOutcomeStore{EventStore: memorystore.New(), fail: session.EventToolFailed}
	pipeline := mustPipeline(t, registry, &recordingPolicyEngine{}, nil, store)
	call := Call{SessionID: "failed", TurnID: "turn", CallID: "call", ToolName: "echo", Arguments: json.RawMessage(`{}`)}
	_, err := pipeline.Execute(context.Background(), call)
	var execution *ExecutionError
	if err == nil || errors.As(err, &execution) {
		t.Fatalf("persistence failure exposed as recoverable execution: %v", err)
	}
	_, err = pipeline.Execute(context.Background(), call)
	if _, ok := errors.AsType[*OutcomeUnknownError](err); !ok {
		t.Fatalf("partial failure replay not stopped: %v", err)
	}
}

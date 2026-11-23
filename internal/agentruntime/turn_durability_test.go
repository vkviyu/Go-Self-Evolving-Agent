package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/atestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

type pendingTurnFailureStore struct{ *memorystore.Store }

func (store *pendingTurnFailureStore) Append(ctx context.Context, id session.ID, sequence uint64, pending ...session.PendingEvent) ([]session.Event, error) {
	for _, event := range pending {
		if event.Type == session.EventTurnFailed {
			return nil, atestore.ErrPending
		}
	}
	return store.Store.Append(ctx, id, sequence, pending...)
}

func TestEinoUnknownToolOutcomePausesDespitePendingFailureAppend(t *testing.T) {
	store := memorystore.New()
	ateUnknown := &client.OutcomeUnknownError{Cause: context.DeadlineExceeded}
	unknown := &tool.OutcomeUnknownError{CallID: "model-call-1", Tool: "echo", Cause: ateUnknown}
	fake := &scriptedModel{}
	calls := 0
	factory := profileFactory(t, store, fake, profileBinding("echo", tool.RiskWrite,
		func(context.Context, json.RawMessage) (json.RawMessage, error) {
			calls++
			return nil, unknown
		}))
	turn, err := factory.NewTurn(t.Context(), "unknown-session")
	if err != nil {
		t.Fatal(err)
	}
	// Model/tool accounting still works, but the failure boundary cannot append
	// while the original ATE write is awaiting reconciliation.
	turn.events = &pendingTurnFailureStore{Store: store}
	_, err = turn.Run(t.Context(), []*schema.Message{schema.UserMessage("echo hello")}, nil)
	if kind, ok := orchestration.ExecutionStopKindOf(err); !ok || kind != orchestration.ExecutionOutcomeUnknown {
		t.Fatalf("runtime did not pause unknown outcome: %v", err)
	}
	var gotUnknown *tool.OutcomeUnknownError
	var gotATE *client.OutcomeUnknownError
	if !errors.As(err, &gotUnknown) || gotUnknown != unknown || !errors.As(err, &gotATE) || gotATE != ateUnknown {
		t.Fatalf("typed causes lost: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, atestore.ErrPending) || !strings.Contains(err.Error(), "[NodeRunError]") {
		t.Fatalf("Eino wrapper or joined causes lost: %v", err)
	}
	if calls != 1 || fake.calls != 1 {
		t.Fatalf("execution continued after uncertainty: tools=%d model=%d", calls, fake.calls)
	}
}

func TestTurnFailureClassifiesOnlyTypedUnknownOutcomes(t *testing.T) {
	unknown := &tool.OutcomeUnknownError{Tool: "write", CallID: "call", Cause: context.Canceled}
	existing := &orchestration.ExecutionStop{Kind: orchestration.ExecutionOutcomeUnknown, Cause: unknown}
	for _, test := range []struct {
		name string
		err  error
		kind orchestration.ExecutionStopKind
	}{
		{"wrapped-and-joined", fmt.Errorf("stream: %w", errors.Join(atestore.ErrPending, fmt.Errorf("tool node: %w", unknown))), orchestration.ExecutionOutcomeUnknown},
		{"already-classified", fmt.Errorf("runner: %w", existing), orchestration.ExecutionOutcomeUnknown},
		{"transport-with-unknown", &orchestration.ExecutionStop{Kind: orchestration.ExecutionTransport, Cause: unknown}, orchestration.ExecutionOutcomeUnknown},
		{"transport", orchestration.FromModelExecutionError(context.DeadlineExceeded), orchestration.ExecutionTransport},
		{"deadline", fmt.Errorf("tool node: %w", context.DeadlineExceeded), ""},
		{"cancelled", context.Canceled, ""},
		{"pending-only", atestore.ErrPending, ""},
		{"ordinary", errors.New("tool outcome unknown text without a typed marker"), ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			turn := &Turn{events: memorystore.New(), sessionID: "s", turnID: "t"}
			err := turn.fail(t.Context(), test.err)
			kind, paused := orchestration.ExecutionStopKindOf(err)
			if kind != test.kind || paused != (test.kind != "") {
				t.Fatalf("classification = %q, %v; want %q: %v", kind, paused, test.kind, err)
			}
			if !errors.Is(err, test.err) {
				t.Fatalf("original error lost: %v", err)
			}
			if test.kind == orchestration.ExecutionOutcomeUnknown {
				var got *tool.OutcomeUnknownError
				if !errors.As(err, &got) || got != unknown || !errors.Is(err, context.Canceled) {
					t.Fatalf("unknown cause lost: %v", err)
				}
			}
			if test.name == "already-classified" {
				var stop *orchestration.ExecutionStop
				if !errors.As(err, &stop) || stop != existing {
					t.Fatalf("existing stop was replaced: %v", err)
				}
			}
		})
	}
}

func TestTurnFailureMarksAvailableCheckpointRecoverable(t *testing.T) {
	store := memorystore.New()
	checkpointID := checkpointID(session.ID("checkpoint-session"), "turn-1")
	if err := store.Set(context.Background(), checkpointID, []byte("opaque checkpoint")); err != nil {
		t.Fatal(err)
	}
	turn := &Turn{
		events:       store,
		sessionID:    "checkpoint-session",
		turnID:       "turn-1",
		checkpointID: checkpointID,
		checkpoints:  store,
	}
	cause := errors.New("provider stream interrupted")
	if err := turn.fail(context.Background(), cause); !errors.Is(err, cause) {
		t.Fatalf("turn failure = %v, want cause", err)
	}
	payload := loadTurnFailurePayload(t, store, "checkpoint-session")
	if !payload.Recoverable || payload.CheckpointID != checkpointID {
		t.Fatalf("failure payload = %+v, want recoverable checkpoint", payload)
	}
}

func TestTurnFailureRemainsNonRecoverableWhenCheckpointMissing(t *testing.T) {
	store := memorystore.New()
	turn := &Turn{
		events:       store,
		sessionID:    "missing-checkpoint-session",
		turnID:       "turn-2",
		checkpointID: checkpointID(session.ID("missing-checkpoint-session"), "turn-2"),
		checkpoints:  store,
	}
	if err := turn.fail(context.Background(), errors.New("synthetic crash")); err == nil {
		t.Fatal("turn failure unexpectedly returned nil")
	}
	payload := loadTurnFailurePayload(t, store, "missing-checkpoint-session")
	if payload.Recoverable {
		t.Fatalf("failure payload unexpectedly recoverable: %+v", payload)
	}
}

type turnFailurePayload struct {
	Recoverable  bool   `json:"recoverable"`
	CheckpointID string `json:"checkpoint_id"`
}

func loadTurnFailurePayload(t *testing.T, store *memorystore.Store, sessionID session.ID) turnFailurePayload {
	t.Helper()
	events, err := store.Load(context.Background(), sessionID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type != session.EventTurnFailed {
			continue
		}
		var payload turnFailurePayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		return payload
	}
	t.Fatal("turn.failed event not found")
	return turnFailurePayload{}
}

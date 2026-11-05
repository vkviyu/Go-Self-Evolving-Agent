package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
)

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

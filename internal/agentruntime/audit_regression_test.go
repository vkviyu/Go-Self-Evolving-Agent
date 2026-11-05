package agentruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
)

type auditFailureStore struct {
	*memorystore.Store
	batches [][]session.EventType
	reject  session.EventType
}

func (store *auditFailureStore) Append(ctx context.Context, id session.ID, sequence uint64, pending ...session.PendingEvent) ([]session.Event, error) {
	types := make([]session.EventType, len(pending))
	for index, event := range pending {
		types[index] = event.Type
		if event.Type == store.reject {
			return nil, errors.New("injected append failure")
		}
	}
	store.batches = append(store.batches, types)
	return store.Store.Append(ctx, id, sequence, pending...)
}

func TestFailureAndCheckpointFactsCommitInOneAppend(t *testing.T) {
	store := &auditFailureStore{Store: memorystore.New()}
	if err := store.Set(context.Background(), "cp", []byte("checkpoint")); err != nil {
		t.Fatal(err)
	}
	turn := &Turn{events: store, checkpoints: store, sessionID: "s", turnID: "t", checkpointID: "cp"}
	cause := errors.New("interrupted")
	if err := turn.fail(context.Background(), cause); !errors.Is(err, cause) {
		t.Fatal(err)
	}
	if len(store.batches) != 1 || len(store.batches[0]) != 2 || store.batches[0][0] != session.EventTurnFailed || store.batches[0][1] != session.EventCheckpointCreated {
		t.Fatalf("failure facts were not atomic: %+v", store.batches)
	}
}

func TestCompletionAppendFailureRetainsRecoverableCheckpoint(t *testing.T) {
	store := &auditFailureStore{Store: memorystore.New(), reject: session.EventTurnCompleted}
	if err := store.Set(context.Background(), "cp", []byte("checkpoint")); err != nil {
		t.Fatal(err)
	}
	turn := &Turn{events: store, checkpoints: store, sessionID: "s", turnID: "t", checkpointID: "cp"}
	iterator, generator := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	generator.Send(&adk.AgentEvent{Output: &adk.AgentOutput{MessageOutput: &adk.MessageVariant{Message: schema.AssistantMessage("done", nil)}}})
	generator.Close()
	if _, err := turn.consumeIterator(context.Background(), iterator, nil, false); err == nil {
		t.Fatal("completion persistence failure ignored")
	}
	if payload := loadTurnFailurePayload(t, store.Store, "s"); !payload.Recoverable || payload.CheckpointID != "cp" {
		t.Fatalf("recoverability lost: %+v", payload)
	}
	if _, found, err := store.Get(context.Background(), "cp"); err != nil || !found {
		t.Fatalf("checkpoint was deleted: found=%v err=%v", found, err)
	}
}

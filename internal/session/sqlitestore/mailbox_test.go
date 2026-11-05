package sqlitestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

func TestMailboxEnqueueDeliveryAndIdempotency(t *testing.T) {
	database, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: "mailbox-root"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.CreateChild(context.Background(), orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker-v1"})
	if err != nil {
		t.Fatal(err)
	}
	spec := orchestration.MessageSpec{
		TeamID: root.TeamID, SenderAgentID: root.AgentID, RecipientAgentID: child.ID,
		Type: orchestration.MessageDirective, Body: json.RawMessage(`{"instruction":"inspect"}`), IdempotencyKey: "message-1",
	}
	message, err := store.Enqueue(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if message.Sequence != 1 || message.DeliveryState != orchestration.DeliveryEnqueued {
		t.Fatalf("unexpected enqueued message: %#v", message)
	}
	repeated, err := store.Enqueue(context.Background(), spec)
	if err != nil || repeated.ID != message.ID {
		t.Fatalf("idempotent enqueue failed: repeated=%#v err=%v", repeated, err)
	}
	conflicting := spec
	conflicting.Body = json.RawMessage(`{"instruction":"different"}`)
	if _, err := store.Enqueue(context.Background(), conflicting); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("got %v, want mailbox idempotency conflict", err)
	}
	messages, err := store.ListMessages(context.Background(), orchestration.MessageQuery{TeamID: root.TeamID, RecipientAgentID: child.ID, States: []orchestration.DeliveryState{orchestration.DeliveryEnqueued}})
	if err != nil || len(messages) != 1 || messages[0].ID != message.ID {
		t.Fatalf("unexpected pending messages: %#v err=%v", messages, err)
	}
	delivered, err := store.MarkDelivered(context.Background(), message.ID)
	if err != nil || delivered.DeliveryState != orchestration.DeliveryDelivered || delivered.DeliveredAt == nil {
		t.Fatalf("delivery transition failed: %#v err=%v", delivered, err)
	}
	acknowledged, err := store.MarkAcknowledged(context.Background(), message.ID)
	if err != nil || acknowledged.DeliveryState != orchestration.DeliveryAcknowledged || acknowledged.AcknowledgedAt == nil {
		t.Fatalf("acknowledgement transition failed: %#v err=%v", acknowledged, err)
	}
	repeatedAck, err := store.MarkAcknowledged(context.Background(), message.ID)
	if err != nil || repeatedAck.DeliveryState != orchestration.DeliveryAcknowledged {
		t.Fatalf("acknowledgement was not idempotent: %#v err=%v", repeatedAck, err)
	}
	if _, err := store.MarkDelivered(context.Background(), message.ID); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("got %v, want invalid transition", err)
	}
}

func TestMailboxReservesUniqueRecipientSequences(t *testing.T) {
	database, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: "mailbox-sequences"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.CreateChild(context.Background(), orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker-v1"})
	if err != nil {
		t.Fatal(err)
	}
	const count = 16
	results := make(chan orchestration.Message, count)
	errorsCh := make(chan error, count)
	var waitGroup sync.WaitGroup
	for index := range count {
		waitGroup.Add(1)
		go func(index int) {
			defer waitGroup.Done()
			message, enqueueErr := store.Enqueue(context.Background(), orchestration.MessageSpec{
				TeamID: root.TeamID, SenderAgentID: root.AgentID, RecipientAgentID: child.ID,
				Type: orchestration.MessageStatus, Body: json.RawMessage(fmt.Sprintf(`{"index":%d}`, index)),
			})
			if enqueueErr != nil {
				errorsCh <- enqueueErr
				return
			}
			results <- message
		}(index)
	}
	waitGroup.Wait()
	close(results)
	close(errorsCh)
	for enqueueErr := range errorsCh {
		t.Fatal(enqueueErr)
	}
	seen := make(map[uint64]bool, count)
	for message := range results {
		if message.Sequence < 1 || message.Sequence > count || seen[message.Sequence] {
			t.Fatalf("invalid duplicate mailbox sequence: %#v", message)
		}
		seen[message.Sequence] = true
	}
	if len(seen) != count {
		t.Fatalf("got %d unique sequences, want %d", len(seen), count)
	}
}

package sqlitestore

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

func TestMailboxLedgerClaimAcknowledgeAndRelease(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "ledger-root"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.CreateChild(ctx, orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker-v1"})
	if err != nil {
		t.Fatal(err)
	}
	enqueue := func(index int) orchestration.Message {
		t.Helper()
		message, err := store.Enqueue(ctx, orchestration.MessageSpec{
			TeamID: root.TeamID, SenderAgentID: child.ID, RecipientAgentID: root.AgentID,
			Type: orchestration.MessageStatus, Body: json.RawMessage(fmt.Sprintf(`{"index":%d}`, index)),
		})
		if err != nil {
			t.Fatal(err)
		}
		return message
	}
	first, second := enqueue(1), enqueue(2)
	// A message delivered without a binding by the legacy path stays available.
	if _, err := store.MarkDelivered(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	claim := orchestration.MessageClaim{TeamID: root.TeamID, RecipientAgentID: root.AgentID, TurnID: "turn-a", Request: 1}
	claimed, err := store.ClaimMessages(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 2 || claimed[0].ID != first.ID || claimed[1].ID != second.ID {
		t.Fatalf("unexpected claim: %#v", claimed)
	}
	for _, message := range claimed {
		if message.ConsumerTurnID != "turn-a" || message.ConsumerRequest != 1 || message.BoundAt == nil || message.DeliveredAt == nil || message.Available() {
			t.Fatalf("message was not bound: %#v", message)
		}
	}
	// Another turn cannot claim bound messages, while the binding turn sees them again.
	other, err := store.ClaimMessages(ctx, orchestration.MessageClaim{TeamID: root.TeamID, RecipientAgentID: root.AgentID, TurnID: "turn-b"})
	if err != nil || len(other) != 0 {
		t.Fatalf("bound messages leaked to another turn: %#v err=%v", other, err)
	}
	third := enqueue(3)
	reclaimed, err := store.ClaimMessages(ctx, orchestration.MessageClaim{TeamID: root.TeamID, RecipientAgentID: root.AgentID, TurnID: "turn-a", Request: 2})
	if err != nil || len(reclaimed) != 3 {
		t.Fatalf("unexpected re-claim: %#v err=%v", reclaimed, err)
	}
	if reclaimed[0].ConsumerRequest != 1 || reclaimed[2].ID != third.ID || reclaimed[2].ConsumerRequest != 2 {
		t.Fatalf("re-claim changed existing bindings: %#v", reclaimed)
	}
	released, err := store.ReleaseTurn(ctx, root.TeamID, root.AgentID, "turn-a")
	if err != nil || released != 3 {
		t.Fatalf("release = %d err=%v", released, err)
	}
	pending, err := store.ListMessages(ctx, orchestration.MessageQuery{TeamID: root.TeamID, RecipientAgentID: root.AgentID})
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range pending {
		if !message.Available() || message.BoundAt != nil || message.ConsumerRequest != 0 {
			t.Fatalf("released message is still bound: %#v", message)
		}
	}
	claimed, err = store.ClaimMessages(ctx, orchestration.MessageClaim{TeamID: root.TeamID, RecipientAgentID: root.AgentID, TurnID: "turn-b", Request: 1})
	if err != nil || len(claimed) != 3 {
		t.Fatalf("released messages were not claimable: %#v err=%v", claimed, err)
	}
	acknowledged, err := store.AcknowledgeTurn(ctx, root.TeamID, root.AgentID, "turn-b")
	if err != nil || acknowledged != 3 {
		t.Fatalf("acknowledge = %d err=%v", acknowledged, err)
	}
	repeated, err := store.AcknowledgeTurn(ctx, root.TeamID, root.AgentID, "turn-b")
	if err != nil || repeated != 0 {
		t.Fatalf("repeated acknowledge = %d err=%v", repeated, err)
	}
	if released, err := store.ReleaseTurn(ctx, root.TeamID, root.AgentID, "turn-b"); err != nil || released != 0 {
		t.Fatalf("release after acknowledge = %d err=%v", released, err)
	}
	remaining, err := store.ClaimMessages(ctx, orchestration.MessageClaim{TeamID: root.TeamID, RecipientAgentID: root.AgentID, TurnID: "turn-c"})
	if err != nil || len(remaining) != 0 {
		t.Fatalf("acknowledged messages were claimable: %#v err=%v", remaining, err)
	}
	events, err := store.LoadStream(ctx, orchestration.StreamMailbox, string(root.AgentID), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[orchestration.EventType]int{}
	for _, event := range events {
		counts[event.Type]++
	}
	if counts[orchestration.EventMailboxBound] != 6 || counts[orchestration.EventMailboxReleased] != 3 || counts[orchestration.EventMailboxAcknowledged] != 3 {
		t.Fatalf("unexpected ledger events: %v", counts)
	}
	if _, err := store.ClaimMessages(ctx, orchestration.MessageClaim{TeamID: root.TeamID, RecipientAgentID: root.AgentID}); err == nil {
		t.Fatal("claim without a turn ID was accepted")
	}
}

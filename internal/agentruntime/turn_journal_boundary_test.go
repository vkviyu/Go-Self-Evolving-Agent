package agentruntime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func TestJournalRejectsChangedInputBeforePreparationOrDispatch(t *testing.T) {
	for _, boundary := range []string{"confirmed_response", "prepared_request"} {
		t.Run(boundary, func(t *testing.T) {
			ctx, store, fake := context.Background(), memorystore.New(), &journalOutcomeModel{}
			factory := profileFactory(t, store, fake)
			mailbox := &fakeBackgroundMailbox{queued: []orchestration.Message{backgroundMessage("new")}}
			factory = factory.WithMailbox(mailbox)
			original := []*schema.Message{schema.UserMessage("original input")}
			receipt := modelReceipt{Version: 2, RawDigest: rawRequestDigest(original), Covered: len(original), Digest: requestPrefixDigest(original), Input: original}
			replay := &journalReplay{}
			if boundary == "confirmed_response" {
				receipt.Response = schema.AssistantMessage("confirmed decision", nil)
				replay.Responses = []modelReceipt{receipt}
			} else {
				replay.Pending = &receipt
			}
			recorded := &recordingModel{inner: fake, factory: factory, events: store, sessionID: "changed-input", turnID: "turn", replay: replay}
			_, err := recorded.Generate(ctx, []*schema.Message{schema.UserMessage("changed input")})
			if err == nil || !strings.Contains(err.Error(), "diverged") || fake.calls != 0 || mailbox.claims != 0 {
				t.Fatalf("changed input crossed a durable boundary: calls=%d mailbox=%d err=%v", fake.calls, mailbox.claims, err)
			}
			if sequence, _ := store.CurrentSequence(ctx, "changed-input"); sequence != 0 {
				t.Fatalf("divergence wrote preparation or model request events: %d", sequence)
			}
		})
	}
}

func TestCheckpointWithoutCommittedReplayMarkerUsesOriginalJournal(t *testing.T) {
	ctx, store, fake := context.Background(), memorystore.New(), &journalRecoveryModel{}
	tools := 0
	factory := profileFactory(t, store, fake, profileBinding("read", tool.RiskRead, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		tools++
		return json.RawMessage(`{"value":"same"}`), nil
	}))
	turn, err := factory.NewTurn(ctx, "checkpoint-marker-gap")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := turn.Run(ctx, []*schema.Message{schema.UserMessage("inspect twice")}, nil); err == nil {
		t.Fatal("expected provider interruption")
	}
	// Eino's new checkpoint write reached disk but its host replay marker
	// did not. These bytes must never be passed to Runner.Resume.
	if err := store.Set(ctx, checkpointID("checkpoint-marker-gap", turn.ID()), []byte("new checkpoint without a matching durable marker")); err != nil {
		t.Fatal(err)
	}
	resumed, err := factory.ResumeTurn(ctx, "checkpoint-marker-gap", turn.ID())
	if err != nil || resumed.journal == nil {
		t.Fatalf("uncommitted checkpoint marker did not fall back to the journal: turn=%+v err=%v", resumed, err)
	}
	result, err := resumed.Resume(ctx, nil)
	if err != nil || result.Final == nil || result.Final.Content != "recovered after confirmed work" || fake.calls != 4 || tools != 2 {
		t.Fatalf("journal fallback repeated work or read the invalid checkpoint: result=%+v calls=%d tools=%d err=%v", result, fake.calls, tools, err)
	}
}

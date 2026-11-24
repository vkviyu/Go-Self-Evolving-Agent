package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func TestResumeForegroundSelectsByDurableState(t *testing.T) {
	store := &historyFailureStore{Store: memorystore.New()}
	chat, model := executionFixture(t, store)
	ctx := t.Context()
	appendTurn := func(turnID, input string, metadata any) {
		t.Helper()
		if _, err := store.Append(ctx, chat.ID(), session.AnySequence,
			session.PendingEvent{Type: session.EventTurnStarted, CorrelationID: turnID, Payload: session.MustJSONPayload(metadata)},
			session.PendingEvent{Type: session.EventUserMessage, CorrelationID: turnID, Payload: session.MustJSONPayload(map[string]string{"content": input})},
		); err != nil {
			t.Fatal(err)
		}
	}
	appendTurn("original", "继续好吗？", struct{}{})
	appendTurn("wake", "agent mailbox message", map[string]string{"input": agentruntime.TurnInputBackground})
	appendTurn("child", "delegated work", map[string]string{"run_id": "child-run"})
	loadTarget := func() (agentruntime.TurnRecord, error) {
		t.Helper()
		records, err := chat.factory.TurnRecords(ctx, chat.ID())
		if err != nil {
			t.Fatal(err)
		}
		return foregroundRecoveryTarget(records)
	}
	if target, err := loadTarget(); err != nil || target.TurnID != "original" {
		t.Fatalf("target=%q err=%v", target.TurnID, err)
	}
	appendTurn("independent", "继续好吗？", struct{}{})
	if _, err := chat.ResumeTurn(ctx, "", nil); !errors.Is(err, ErrAmbiguousRecovery) || model.calls.Load() != 0 {
		t.Fatalf("ambiguous recovery dispatched work: calls=%d err=%v", model.calls.Load(), err)
	}
	if err := chat.factory.SelectForegroundTurn(ctx, chat.ID(), "original"); err != nil {
		t.Fatal(err)
	}
	if target, err := loadTarget(); err != nil || target.TurnID != "original" || target.ForegroundSequence == 0 {
		t.Fatalf("persisted target=%+v err=%v", target, err)
	}
	if _, err := store.Append(ctx, chat.ID(), session.AnySequence, session.PendingEvent{
		Type: session.EventTurnCompleted, CorrelationID: "original", Payload: []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := chat.ResumeTurn(ctx, "", nil); !errors.Is(err, ErrNoForegroundRecovery) || model.calls.Load() != 0 {
		t.Fatalf("completed selection reopened older work: calls=%d err=%v", model.calls.Load(), err)
	}
}

func TestRecoveriesInspectEachInterruptedSnapshotOnce(t *testing.T) {
	turns := []agentruntime.TurnRecord{
		{TurnID: "old-a", HasJournal: true, RuntimeSnapshotID: "previous", OpeningPreview: "count lines"},
		{TurnID: "old-b", HasCheckpoint: true, RuntimeSnapshotID: "previous", OpeningPreview: "finish review"},
		{TurnID: "new", HasJournal: true, RuntimeSnapshotID: "current"},
		{TurnID: "done", Completed: true, RuntimeSnapshotID: "irrelevant"},
	}
	checks := make(map[string]int)
	items, err := recoveriesForTurns(t.Context(), turns, func(_ context.Context, id string) error {
		checks[id]++
		if id == "previous" {
			return tool.ErrSnapshotHostChanged
		}
		return nil
	})
	if err != nil || len(items) != 3 || len(checks) != 2 || checks["previous"] != 1 || checks["current"] != 1 {
		t.Fatalf("checks=%v items=%+v err=%v", checks, items, err)
	}
	for i, item := range items[:2] {
		if item.Phase != "unavailable" || !strings.Contains(item.Reason, "original GSEA executable") || item.Prompt != turns[i].OpeningPreview {
			t.Fatalf("old turn presented without its request and blocker: %+v", item)
		}
	}
	if items[2].Phase != "execution" {
		t.Fatalf("current turn unavailable: %+v", items[2])
	}
}

func TestFollowUpUsesRefreshedSessionHistory(t *testing.T) {
	ctx := t.Context()
	store := &historyFailureStore{Store: memorystore.New()}
	chat, model := executionFixture(t, store)
	if _, err := chat.RunTurn(ctx, "first question", nil); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, chat.factory, chat.ID())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chat.RunTurn(ctx, "later confirmed question", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(ctx, chat.ID(), session.AnySequence,
		session.PendingEvent{Type: session.EventTurnStarted, CorrelationID: "pending", Payload: []byte(`{"foreground":true}`)},
		session.PendingEvent{Type: session.EventUserMessage, CorrelationID: "pending", Payload: []byte(`{"content":"separate interrupted work"}`)},
	); err != nil {
		t.Fatal(err)
	}
	const followUp = "接着做，但排除 vendor"
	result, err := reopened.RunTurn(ctx, followUp, nil)
	if err != nil || model.calls.Load() != 3 {
		t.Fatalf("follow-up calls=%d err=%v", model.calls.Load(), err)
	}
	// Inspect the exact inputs frozen for the new turn, not the view refreshed
	// after the reply. An early copy would omit the second question and answer.
	found := false
	err = session.VisitEvents(ctx, store, chat.ID(), func(event session.Event) error {
		if event.Type != session.EventTurnStarted || event.CorrelationID != result.TurnID {
			return nil
		}
		found = true
		var start struct {
			Journal struct{ Input []*schema.Message }
		}
		if err := json.Unmarshal(event.Payload, &start); err != nil {
			return err
		}
		var messages []string
		for _, message := range start.Journal.Input {
			messages = append(messages, message.Content)
		}
		if len(messages) != 6 || strings.Join(messages[:4], "|") != "first question|answer|later confirmed question|answer" ||
			!strings.Contains(messages[4], "separate interrupted work") || messages[5] != followUp {
			t.Errorf("turn journal input = %q", messages)
		}
		return nil
	})
	if err != nil || !found {
		t.Fatalf("follow-up start found=%v err=%v", found, err)
	}
}

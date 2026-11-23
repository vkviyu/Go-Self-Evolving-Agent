package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateprocess"
)

func TestProgressKeepsOperationAndIndependentStorageWaits(t *testing.T) {
	model, err := NewModel(context.Background(), Config{Conversation: &fakeConversation{id: "progress"}})
	if err != nil {
		t.Fatal(err)
	}
	model.busy = true
	start := time.Now().Add(-10 * time.Second)
	model.Update(runtimeEventMsg{event: agentruntime.Event{Type: agentruntime.EventModelRequestPersistenceStarted}, at: start})
	model.applyStorageProgress(storageProgressMsg{progress: ateprocess.Progress{BatchID: 1, Stage: ateprocess.StageOwnership, At: start.Add(2 * time.Second)}})
	model.applyStorageProgress(storageProgressMsg{progress: ateprocess.Progress{BatchID: 2, Stage: ateprocess.StageQueue, At: start.Add(4 * time.Second)}})
	model.applyStorageProgress(storageProgressMsg{progress: ateprocess.Progress{BatchID: 2, Stage: ateprocess.StageDone, At: start.Add(5 * time.Second)}})
	label := model.progressLabel(start.Add(9 * time.Second))
	if label != "Persisting request · 9s · ATE waiting for lock 7s" {
		t.Fatalf("operation or remaining wait was lost: %q", label)
	}
	model.applyStorageProgress(storageProgressMsg{progress: ateprocess.Progress{BatchID: 1, Stage: ateprocess.StageDone, At: start.Add(9 * time.Second)}})
	if label := model.progressLabel(start.Add(10 * time.Second)); label != "Persisting request · 10s" {
		t.Fatalf("completed storage wait remains: %q", label)
	}
	model.Update(runtimeEventMsg{event: agentruntime.Event{Type: agentruntime.EventAssistantTextDelta, Delta: "a"}, at: start.Add(10 * time.Second)})
	model.Update(runtimeEventMsg{event: agentruntime.Event{Type: agentruntime.EventAssistantTextDelta, Delta: "b"}, at: start.Add(12 * time.Second)})
	if label := model.progressLabel(start.Add(13 * time.Second)); label != "Responding · 3s" {
		t.Fatalf("streaming reset the stage clock: %q", label)
	}
	model.resetProgress()
	if len(model.storageProgress) != 0 || !model.stageStartedAt.After(start) {
		t.Fatal("new operation retained old timing")
	}
}

func TestProgressStatusFitsNarrowTerminals(t *testing.T) {
	model, err := NewModel(context.Background(), Config{Conversation: &fakeConversation{id: "progress"}})
	if err != nil {
		t.Fatal(err)
	}
	model.busy = true
	model.stage = "Persisting request"
	model.resetProgress()
	model.applyStorageProgress(storageProgressMsg{progress: ateprocess.Progress{BatchID: 1, Stage: ateprocess.StageHandshake, At: time.Now()}})
	for _, width := range []int{24, 40, 80, 120} {
		view := model.renderStatus(width)
		if ansi.StringWidth(view) > width {
			t.Fatalf("status exceeds width %d: %s", width, view)
		}
		if width >= 80 && !strings.Contains(ansi.Strip(view), "handshake") {
			t.Fatalf("storage detail missing: %s", view)
		}
	}
}

func TestStorageObserverDoesNotBlockCancelledUI(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	events := make(chan tea.Msg)
	observed := observeStorage(ctx, ctx, events)
	manager, err := ateprocess.New(ateprocess.Options{Binary: "/nonexistent/ate", Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = manager.WithClient(observed, func(*client.Client) error {
			t.Error("cancelled batch invoked its callback")
			return nil
		})
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled observer blocked the operation")
	}
}

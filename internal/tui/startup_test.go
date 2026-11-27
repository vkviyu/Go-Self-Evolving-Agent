package tui

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateprocess"
)

type startupUIFake struct {
	fakeConversation
	calls atomic.Int32
}

func (conversation *startupUIFake) RunTurn(ctx context.Context, input string, sink agentruntime.EventSink) (agentruntime.Result, error) {
	conversation.calls.Add(1)
	if err := sink(ctx, agentruntime.Event{Type: agentruntime.EventAssistantTextDelta, Delta: "forwarded answer"}); err != nil {
		return agentruntime.Result{}, err
	}
	return agentruntime.Result{Final: schema.AssistantMessage("forwarded answer", nil)}, nil
}

func (*startupUIFake) ContextUsage() agentruntime.ContextUsage {
	return agentruntime.ContextUsage{Used: 10}
}

func (*startupUIFake) ContextUsageFor(input string) agentruntime.ContextUsage {
	return agentruntime.ContextUsage{Used: 10 + len(input)}
}

func (*startupUIFake) Compress(context.Context) (bool, agentruntime.ContextUsage, error) {
	return false, agentruntime.ContextUsage{}, nil
}

// executeStartupCommands models Bubble Tea's concurrent Batch execution. The
// test drives Update itself, making lost events or competing waiters time out.
func executeStartupCommands(ctx context.Context, command tea.Cmd, messages chan<- tea.Msg) {
	if command == nil {
		return
	}
	go func() {
		message := command()
		if batch, ok := message.(tea.BatchMsg); ok {
			for _, child := range batch {
				executeStartupCommands(ctx, child, messages)
			}
			return
		}
		if message != nil {
			select {
			case messages <- message:
			case <-ctx.Done():
			}
		}
	}()
}

func TestStartupQueuesUntilReadyHydratesHistoryAndForwardsEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	states := make(chan StartupState, 8)
	conversation := &startupUIFake{fakeConversation: fakeConversation{id: "startup"}}
	model, err := NewModel(ctx, Config{Conversation: conversation, Startup: states})
	if err != nil {
		t.Fatal(err)
	}
	model.resize(180, 30)
	messages := make(chan tea.Msg, 32)
	executeStartupCommands(ctx, model.Init(), messages)
	model.input.SetValue("queued question")
	executeStartupCommands(ctx, model.submit(), messages)

	started := time.Now().Add(-58 * time.Second)
	states <- StartupState{Phase: StartupMaintenance, At: started}
	states <- StartupState{Storage: &ateprocess.Progress{BatchID: 1, Stage: ateprocess.StageHandshake, At: started}}
	for model.startupPhase != StartupMaintenance || len(model.startupStorage) == 0 {
		select {
		case message := <-messages:
			_, command := model.Update(message)
			executeStartupCommands(ctx, command, messages)
		case <-ctx.Done():
			t.Fatal("Init did not consume startup progress")
		}
	}
	if conversation.calls.Load() != 0 || model.startupOperation == nil {
		t.Fatal("request crossed the startup barrier")
	}
	view := ansi.Strip(model.View().Content)
	for _, want := range []string{"Waiting for runtime startup", "Session maintenance", "58s", "ATE opening store / handshake"} {
		if !strings.Contains(view, want) {
			t.Fatalf("missing startup detail %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "Starting request") {
		t.Fatal("startup wait was presented as a model request")
	}

	answer := schema.AssistantMessage("old answer", nil)
	answer.ResponseMeta = &schema.ResponseMeta{Usage: &schema.TokenUsage{PromptTokens: 9, CompletionTokens: 3, TotalTokens: 12}}
	conversation.history = []*schema.Message{schema.UserMessage("old question"), answer}
	transcript := append([]*schema.Message(nil), conversation.history...)
	transcript = append(transcript, schema.UserMessage("interrupted question"), schema.AssistantMessage("partial answer", nil))
	control, transcripts := &fakeAgentControl{}, &fakeTranscriptReader{}
	activity := fakeActivityProvider{}
	states <- StartupState{
		Ready: true, At: time.Now(), TeamID: "team-startup",
		Agents: control, Activity: activity, Transcripts: transcripts, RootTranscript: transcript,
	}
	close(states)
	seenEvent, done := false, false
	for !done {
		select {
		case message := <-messages:
			_, isEvent := message.(runtimeEventMsg)
			seenEvent = seenEvent || isEvent
			_, done = message.(turnDoneMsg)
			_, command := model.Update(message)
			if state, ok := message.(StartupMsg); ok && state.State.Ready {
				if model.startup != nil || model.teamID != "team-startup" || model.completedTurns != 1 || model.lastUsage.Total != 12 {
					t.Fatal("final startup attachment was not applied")
				}
				if model.agentControl != control || model.transcripts != transcripts || model.activityProvider != activity {
					t.Fatal("startup capabilities were not attached")
				}
				if model.contextUsage.Used != 10+len("queued question") {
					t.Fatal("pending context usage was not hydrated")
				}
				if len(model.blocks) != 5 || model.blocks[0].content != "old question" || model.blocks[2].content != "interrupted question" ||
					model.blocks[3].content != "partial answer" || model.blocks[4].content != "queued question" {
					t.Fatalf("history hydration lost/duplicated the queued question: %+v", model.blocks)
				}
				if len(model.inputHistory) != 3 || model.inputHistory[0] != "old question" || model.inputHistory[1] != "interrupted question" {
					t.Fatalf("input history was not hydrated: %v", model.inputHistory)
				}
				if model.sessionTitle != "old question" {
					t.Fatalf("queued input replaced the restored session title: %s", model.sessionTitle)
				}
			}
			executeStartupCommands(ctx, command, messages)
		case <-ctx.Done():
			t.Fatal("queued turn or event waiter stalled")
		}
	}
	if !seenEvent || conversation.calls.Load() != 1 || model.busy || model.completedTurns != 2 {
		t.Fatalf("turn lifecycle mismatch: events=%v calls=%d busy=%v turns=%d", seenEvent, conversation.calls.Load(), model.busy, model.completedTurns)
	}
	if _, command := model.Update(StartupMsg{State: StartupState{Ready: true}}); command != nil {
		t.Fatal("duplicate terminal message restarted startup")
	}
	if len(model.blocks) != 6 {
		t.Fatalf("duplicate startup hydration: %+v", model.blocks)
	}
}

func TestStartupFailureVisibleWithoutSubmissionAndClosedChannelStops(t *testing.T) {
	for _, closed := range []bool{false, true} {
		states := make(chan StartupState, 1)
		model, err := NewModel(t.Context(), Config{Conversation: &fakeConversation{}, Startup: states})
		if err != nil {
			t.Fatal(err)
		}
		model.resize(120, 24)
		if !closed {
			states <- StartupState{Err: errors.New("maintenance failed")}
		}
		close(states)
		_, command := model.Update(waitForStartup(t.Context(), states)())
		if command != nil || model.startup != nil || model.startupErr == nil {
			t.Fatal("terminal startup state did not stop the receive chain")
		}
		if view := ansi.Strip(model.View().Content); !strings.Contains(view, "Runtime startup failed") {
			t.Fatalf("startup error is invisible without user input: %s", view)
		}
		if waitForStartup(t.Context(), model.startup) != nil {
			t.Fatal("nil startup channel still has a waiter")
		}
	}
}

func TestStartupQueuedCancellationAndFailureNeverRunRequest(t *testing.T) {
	for _, fail := range []bool{false, true} {
		conversation := &startupUIFake{}
		model, err := NewModel(t.Context(), Config{Conversation: conversation, Startup: make(chan StartupState)})
		if err != nil {
			t.Fatal(err)
		}
		model.input.SetValue("queued")
		model.submit()
		if fail {
			model.Update(StartupMsg{State: StartupState{Err: errors.New("recovery failed")}})
		} else {
			model.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
			model.Update(StartupMsg{State: StartupState{Ready: true}})
		}
		if model.startupOperation != nil || model.busy || conversation.calls.Load() != 0 {
			t.Fatal("cancelled/failed startup retained a runnable request")
		}
	}
}

func TestStartupPhasesAndStorageUseIndependentTimestamps(t *testing.T) {
	model, err := NewModel(t.Context(), Config{Conversation: &fakeConversation{}, Startup: make(chan StartupState)})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	model.stageStartedAt = start
	for _, phase := range []StartupPhase{StartupModelInitialization, StartupRootOwner, StartupRuntimeFactory, StartupSupervisor, StartupSessionMailbox, StartupMaintenance} {
		model.Update(StartupMsg{State: StartupState{Phase: phase, At: start.Add(time.Second)}})
		if !strings.Contains(model.progressLabel(start.Add(10*time.Second)), startupPhaseLabel(phase)+" 9s") {
			t.Fatalf("phase %v timestamp was lost", phase)
		}
	}
	for _, event := range []ateprocess.Progress{
		{BatchID: 1, Stage: ateprocess.StageHandshake, At: start.Add(2 * time.Second)},
		{BatchID: 2, Stage: ateprocess.StageQueue, At: start.Add(3 * time.Second)},
		{BatchID: 2, Stage: ateprocess.StageDone, At: start.Add(4 * time.Second)},
	} {
		model.Update(StartupMsg{State: StartupState{Storage: &event}})
	}
	if got := model.progressLabel(start.Add(10 * time.Second)); got != "Starting runtime · 10s · Session maintenance 9s · ATE opening store / handshake 8s" {
		t.Fatalf("startup timing or concurrent storage completion incorrect: %s", got)
	}
	model.Update(StartupMsg{State: StartupState{Storage: &ateprocess.Progress{BatchID: 1, Stage: ateprocess.StageDone}}})
	if strings.Contains(model.progressLabel(start.Add(11*time.Second)), "ATE") {
		t.Fatal("completed storage stage remains visible")
	}
}

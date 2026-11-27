package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
)

type fakeBackgroundConversation struct {
	*fakeConversation
	pending int
}

func (conversation *fakeBackgroundConversation) PendingBackground(context.Context) (int, error) {
	return conversation.pending, nil
}

func (*fakeBackgroundConversation) RunBackgroundTurn(context.Context, agentruntime.EventSink) (agentruntime.Result, error) {
	return agentruntime.Result{}, nil
}

// newBackgroundModel builds a model whose conversation has pending Agent
// messages; autoContinue selects the wake policy.
func newBackgroundModel(t *testing.T, autoContinue bool, history ...*schema.Message) *Model {
	t.Helper()
	model, err := NewModel(context.Background(), Config{
		Conversation: &fakeBackgroundConversation{fakeConversation: &fakeConversation{id: "session-test", history: history}, pending: 2},
		Model:        "gpt-test",
		AutoContinue: autoContinue,
	})
	if err != nil {
		t.Fatal(err)
	}
	model.resize(100, 30)
	return model
}

// wake runs one idle check and returns whether it started a wake turn.
func wake(t *testing.T, model *Model) bool {
	t.Helper()
	command := model.checkBackground()
	if command == nil {
		t.Fatal("background check returned no command")
	}
	pending, ok := command().(backgroundPendingMsg)
	if !ok {
		return false
	}
	model.handleBackgroundPending(pending)
	return model.busy && model.backgroundRunning
}

func TestWakeTurnStartsForPendingAgentMessages(t *testing.T) {
	model := newBackgroundModel(t, true)
	turns := model.completedTurns
	if !wake(t, model) {
		t.Fatal("idle model with pending messages did not wake")
	}
	last := model.blocks[len(model.blocks)-1]
	if last.kind != blockBackground || !strings.Contains(last.content, "2 Agent messages") {
		t.Fatalf("wake block = %+v", last)
	}
	model.finishTurn(turnDoneMsg{result: agentruntime.Result{Final: schema.AssistantMessage("handled", nil)}})
	if model.completedTurns != turns {
		t.Fatalf("wake turn counted as user turn: %d != %d", model.completedTurns, turns)
	}
	if model.wakeAllowed() {
		t.Fatal("wake allowed again inside the minimum interval")
	}
	model.lastWake = time.Now().Add(-time.Minute)
	if !model.wakeAllowed() {
		t.Fatal("wake not allowed after the minimum interval")
	}
	model.input.SetValue("draft")
	if model.wakeAllowed() {
		t.Fatal("wake allowed while the user writes")
	}
}

func TestWakeBudgetResetsOnUserInput(t *testing.T) {
	model := newBackgroundModel(t, true)
	model.consecutiveWakes = maximumConsecutiveWakes
	if model.wakeAllowed() {
		t.Fatal("wake allowed past the consecutive wake limit")
	}
	model.wakeSuppressed = true
	model.submitInputForTest("continue")
	if model.wakeSuppressed || model.consecutiveWakes != 0 {
		t.Fatalf("user input did not reset the wake budget: suppressed=%v wakes=%d", model.wakeSuppressed, model.consecutiveWakes)
	}
}

func TestCancelledWakeSuppressesFurtherWakes(t *testing.T) {
	model := newBackgroundModel(t, true)
	if !wake(t, model) {
		t.Fatal("model did not wake")
	}
	model.handleKey(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	model.finishTurn(turnDoneMsg{err: context.Canceled})
	model.lastWake = time.Time{}
	if !model.wakeSuppressed || model.wakeAllowed() {
		t.Fatal("cancelled wake did not suppress the next wake")
	}
}

func TestSkippedWakeLeavesNoTrace(t *testing.T) {
	model := newBackgroundModel(t, true)
	before := len(model.blocks)
	if !wake(t, model) {
		t.Fatal("model did not wake")
	}
	model.finishTurn(turnDoneMsg{err: agentruntime.ErrNoBackgroundInput})
	if len(model.blocks) != before || model.stage != "Ready" {
		t.Fatalf("skipped wake left blocks=%d stage=%q", len(model.blocks)-before, model.stage)
	}
}

func TestAutoContinueDisabledNeverWakes(t *testing.T) {
	model := newBackgroundModel(t, false)
	if model.background != nil || model.scheduleBackgroundCheck() != nil {
		t.Fatal("model wakes although auto-continue is disabled")
	}
}

func TestBackgroundInputRendersAsAgentMessages(t *testing.T) {
	input := agentruntime.BackgroundInputHeader + "\n- result from agent-child: {\"summary\":\"tests pass\"}"
	model := newBackgroundModel(t, true,
		schema.UserMessage("typed question"),
		schema.UserMessage(input),
		schema.AssistantMessage("noted", nil),
	)
	if len(model.inputHistory) != 1 || model.inputHistory[0] != "typed question" {
		t.Fatalf("input history = %q", model.inputHistory)
	}
	if model.blocks[1].kind != blockBackground {
		t.Fatalf("background input block kind = %d", model.blocks[1].kind)
	}
	plain := ansi.Strip(model.transcriptContent)
	if !strings.Contains(plain, "Agent messages") || !strings.Contains(plain, "result from agent-child") || strings.Contains(plain, "Untrusted Agent messages") {
		t.Fatalf("background block rendering = %q", plain)
	}
}

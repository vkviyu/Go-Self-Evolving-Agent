package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/conversation"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/atestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

type recoveryUIFake struct {
	fakeConversation
	runs, resumes int
	resumedID     string
}

func TestRecoveryNoticeSummarizesStartupAndShowsOnlyApplicableActions(t *testing.T) {
	model := newTestModel(t)
	items := []conversation.Recovery{
		{TurnID: "journal-turn", Phase: "execution"},
		{TurnID: "unbound", Phase: "unavailable", Reason: "no durable turn"},
	}
	model.showRecoveries(recoveriesMsg{items: items})
	if notice := model.blocks[len(model.blocks)-1]; notice.kind != blockSystem || !strings.Contains(notice.content, "(2) · /recoveries") || strings.Contains(notice.content, "journal-turn") {
		t.Fatalf("startup recovery notice: %#v", notice)
	}
	model.showRecoveries(recoveriesMsg{items: items, explicit: true})
	text := model.blocks[len(model.blocks)-1].content
	if !strings.Contains(text, "turn journal-turn  execution") || !strings.Contains(text, "turn unbound  unavailable — no durable turn") || !strings.Contains(text, "/resume <turn-id>") || strings.Contains(text, "missing checkpoints") {
		t.Fatalf("recovery detail: %s", text)
	}
	model.showRecoveries(recoveriesMsg{items: items[1:], explicit: true})
	if text := model.blocks[len(model.blocks)-1].content; strings.Contains(text, "/resume") {
		t.Fatalf("unavailable work advertised execution: %s", text)
	}
}

func TestTransportPauseShowsRecoveryEntryPoint(t *testing.T) {
	model, err := NewModel(t.Context(), Config{Conversation: &recoveryUIFake{}})
	if err != nil {
		t.Fatal(err)
	}
	model.resize(100, 30)
	model.finishTurn(turnDoneMsg{err: orchestration.FromModelExecutionError(context.DeadlineExceeded)})
	if model.stage != "Paused" || !strings.Contains(model.blocks[len(model.blocks)-1].content, "/recoveries") {
		t.Fatalf("transport stop did not show recovery entry point: stage=%q blocks=%+v", model.stage, model.blocks)
	}
}

func TestUnknownOutcomePausesWithPersistenceReconciliationGuidance(t *testing.T) {
	for _, hasRecovery := range []bool{false, true} {
		for _, classified := range []bool{false, true} {
			for _, cause := range []error{context.DeadlineExceeded, context.Canceled} {
				t.Run(fmt.Sprintf("recovery=%v/classified=%v/cause=%v", hasRecovery, classified, cause), func(t *testing.T) {
					model := newTestModel(t)
					if hasRecovery {
						model.conversation = &recoveryUIFake{}
					}
					unknown := &tool.OutcomeUnknownError{Tool: "write", CallID: "call", Cause: cause}
					var err error = fmt.Errorf("tool node: %w", unknown)
					if classified {
						err = &orchestration.ExecutionStop{Kind: orchestration.ExecutionOutcomeUnknown, Cause: err}
					}
					err = fmt.Errorf("runner: %w", errors.Join(err, atestore.ErrPending))
					model.busy = true
					model.finishTurn(turnDoneMsg{err: err})
					if model.stage != "Paused" || model.busy || model.completedTurns != 0 {
						t.Fatalf("unknown outcome treated as terminal: stage=%q busy=%v completed=%d", model.stage, model.busy, model.completedTurns)
					}
					block := model.blocks[len(model.blocks)-1]
					if block.kind != blockFailure || !strings.Contains(block.content, err.Error()) {
						t.Fatalf("original diagnostic lost: %+v", block)
					}
					for _, want := range []string{
						"Pending persistence needs reconciliation",
						"/recoveries only inspects interrupted work; it does not reconcile saved ATE appends",
						"Close the runtime owning this session before reconciliation",
						"then run: gsea session reconcile <session-id>",
					} {
						if !strings.Contains(block.content, want) {
							t.Fatalf("missing %q in guidance: %s", want, block.content)
						}
					}
				})
			}
		}
	}
}

func TestOrdinaryTurnErrorsDoNotSuggestUnknownOutcomeReconciliation(t *testing.T) {
	for _, test := range []struct {
		name  string
		err   error
		stage string
	}{
		{"deadline", fmt.Errorf("tool node: %w", context.DeadlineExceeded), "Failed"},
		{"pending-only", atestore.ErrPending, "Failed"},
		{"ordinary", errors.New("tool outcome unknown text without a typed marker"), "Failed"},
		{"cancelled", fmt.Errorf("runner: %w", context.Canceled), "Cancelled"},
		{"transport", orchestration.FromModelExecutionError(context.DeadlineExceeded), "Paused"},
	} {
		t.Run(test.name, func(t *testing.T) {
			model := newTestModel(t)
			model.finishTurn(turnDoneMsg{err: test.err})
			if model.stage != test.stage {
				t.Fatalf("stage = %q, want %q", model.stage, test.stage)
			}
			for _, block := range model.blocks {
				if strings.Contains(block.content, "Pending persistence needs reconciliation") || strings.Contains(block.content, "gsea session reconcile") {
					t.Fatalf("ordinary error advertised unknown outcome reconciliation: %s", block.content)
				}
			}
		})
	}
}

func (fake *recoveryUIFake) RunTurn(context.Context, string, agentruntime.EventSink) (agentruntime.Result, error) {
	fake.runs++
	return agentruntime.Result{}, nil
}
func (fake *recoveryUIFake) Recoveries(context.Context) ([]conversation.Recovery, error) {
	return []conversation.Recovery{{TurnID: "original", Phase: "assessment"}}, nil
}
func (fake *recoveryUIFake) ResumeTurn(_ context.Context, id string, _ agentruntime.EventSink) (agentruntime.Result, error) {
	fake.resumes++
	fake.resumedID = id
	return agentruntime.Result{TurnID: id, Final: schema.AssistantMessage("recovered", nil)}, nil
}

func TestRecoveryCommandsDiscoverWithoutRunningAndResumeWithoutNewInput(t *testing.T) {
	fake := &recoveryUIFake{}
	model, err := NewModel(context.Background(), Config{Conversation: fake})
	if err != nil {
		t.Fatal(err)
	}
	model.resize(100, 30)
	command, handled := model.handleCommand("/recoveries")
	if !handled || command == nil {
		t.Fatal("discovery command missing")
	}
	model.Update(command())
	if fake.runs != 0 || fake.resumes != 0 || model.busy {
		t.Fatal("listing executed work")
	}
	if command, _ := model.handleCommand("/resume"); command != nil || model.busy {
		t.Fatal("missing ID started recovery")
	}
	command, handled = model.handleCommand("/resume original")
	if !handled || command == nil || !model.busy || len(model.inputHistory) != 0 {
		t.Fatal("recovery was treated as new user input")
	}
	batch := command().(tea.BatchMsg)
	batch[0]() // Run only the operation; UI waits/spinner are separate commands.
	message := <-model.turnEvents
	model.Update(message)
	if fake.runs != 0 || fake.resumes != 1 || fake.resumedID != "original" || model.busy {
		t.Fatalf("wrong dispatch: %#v", fake)
	}
}

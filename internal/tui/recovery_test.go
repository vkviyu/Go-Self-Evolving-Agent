package tui

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/conversation"
)

type recoveryUIFake struct {
	fakeConversation
	runs, resumes int
	resumedID     string
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

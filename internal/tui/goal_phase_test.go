package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
)

func TestGoalPhasesReplaceThinkingAndExposeExhaustion(t *testing.T) {
	model, err := NewModel(context.Background(), Config{Conversation: &fakeConversation{id: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	model.busy = true
	model.stage = "Thinking"
	for _, tc := range []struct{ stage, want string }{{"executing", "Executing task"}, {"verifying", "Verifying outcome"}, {"retrying", "Continuing task"}} {
		model.applyRuntimeEvent(agentruntime.Event{Type: agentruntime.EventGoalPhase, GoalPhase: &agentruntime.GoalPhase{Stage: tc.stage, Round: 2, MaxRounds: 4}})
		if !strings.Contains(model.stage, tc.want) || !strings.Contains(model.stage, "2/4") {
			t.Fatalf("stage=%q", model.stage)
		}
	}
	model.applyRuntimeEvent(agentruntime.Event{Type: agentruntime.EventGoalPhase, GoalPhase: &agentruntime.GoalPhase{Stage: "stopped", State: "exhausted", Reason: "max_wall_time"}})
	model.finishTurn(turnDoneMsg{result: agentruntime.Result{GoalState: agentgoal.StateExhausted}})
	if model.stage != "Task exhausted" || model.busy {
		t.Fatalf("stage=%q busy=%v", model.stage, model.busy)
	}
	if len(model.blocks) == 0 || !strings.Contains(model.blocks[len(model.blocks)-1].content, "max_wall_time") {
		t.Fatal("exhaustion reason is invisible")
	}
}

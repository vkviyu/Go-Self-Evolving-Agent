package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

type goalOutputConversation struct{}

func (goalOutputConversation) ID() session.ID      { return "test" }
func (goalOutputConversation) CompletedTurns() int { return 0 }
func (goalOutputConversation) RunTurn(ctx context.Context, _ string, sink agentruntime.EventSink) (agentruntime.Result, error) {
	if err := sink(ctx, agentruntime.Event{Type: agentruntime.EventGoalPhase, GoalPhase: &agentruntime.GoalPhase{Stage: "verifying", Round: 1, MaxRounds: 4}}); err != nil {
		return agentruntime.Result{}, err
	}
	if err := sink(ctx, agentruntime.Event{Type: agentruntime.EventGoalPhase, GoalPhase: &agentruntime.GoalPhase{Stage: "stopped", State: "exhausted", Round: 1, MaxRounds: 4, Reason: "max_wall_time"}}); err != nil {
		return agentruntime.Result{}, err
	}
	return agentruntime.Result{Final: schema.AssistantMessage("partial result", nil), GoalID: "goal-test", GoalState: agentgoal.StateExhausted, GoalStopReason: agentgoal.StopMaxWallTime, GoalRounds: 1}, nil
}

func TestHeadlessGoalStatusIsVisibleWithoutCorruptingJSON(t *testing.T) {
	var output bytes.Buffer
	if err := runHeadlessTurnWithFormat(context.Background(), &output, goalOutputConversation{}, "task", headlessFormatJSON); err != nil {
		t.Fatal(err)
	}
	var result headlessJSONResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, output.String())
	}
	if result.Goal == nil || result.Goal.State != "exhausted" || result.Goal.StopReason != "max_wall_time" {
		t.Fatalf("goal=%+v", result.Goal)
	}
	output.Reset()
	if err := runHeadlessTurnWithFormat(context.Background(), &output, goalOutputConversation{}, "task", headlessFormatStream); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"verifying", "exhausted", "max_wall_time", "partial result"} {
		if !strings.Contains(output.String(), text) {
			t.Fatalf("missing %q: %s", text, output.String())
		}
	}
}

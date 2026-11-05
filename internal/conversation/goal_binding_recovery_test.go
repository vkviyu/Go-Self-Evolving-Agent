package conversation

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

func TestAlwaysGoalTurnMissingRoundBindingCannotBypassAcceptanceOnRecovery(t *testing.T) {
	ctx := context.Background()
	runID, _ := orchestration.NewRunID()
	path := filepath.Join(t.TempDir(), "goal-registration-gap.db")
	fake := &goalLoopModel{}
	chat, database := openRecoveryConversation(t, path, fake, nil, runID)
	scope := chat.factory.ActorScope()
	goal, err := chat.goalStore().CreateGoal(ctx, agentgoal.CreateRequest{ID: "not-yet-bound", SessionID: chat.ID(), TeamID: scope.TeamID, AgentID: scope.AgentID, Text: "must satisfy Goal acceptance", Assessment: agentgoal.AssessmentIndependent, MaxRounds: 3, MaxWallTime: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	turnID := ""
	// Simulate termination after turn.started and its journal were persisted,
	// before StartRound could bind that turn to the already-created Goal.
	_, err = chat.runInnerTurn(ctx, goal.Text, agentruntime.ContextHints{GoalID: goal.ID}, nil, func(id string) error { turnID = id; return goalBudgetPauseError() })
	if err == nil || turnID == "" || fake.callCount() != 0 {
		t.Fatalf("gap setup failed: turn=%q err=%v calls=%d", turnID, err, fake.callCount())
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, reopened := openRecoveryConversation(t, path, fake, nil, runID)
	items, err := DiscoverRecoveries(ctx, restarted.ID(), reopened.SessionEvents(), reopened.Checkpoints(), reopened.Orchestration())
	if err != nil {
		t.Fatal(err)
	}
	foundTurn := false
	for _, item := range items {
		if item.TurnID == turnID {
			foundTurn = true
			if item.Phase != "unavailable" || !strings.Contains(item.Reason, "round binding") {
				t.Fatalf("unsafe recovery discovery: %#v", item)
			}
		}
	}
	if !foundTurn {
		t.Fatalf("unbound Goal turn was hidden: %#v", items)
	}
	if _, err := restarted.ResumeTurn(ctx, turnID, nil); err == nil || !strings.Contains(err.Error(), "round binding") {
		t.Fatalf("turn recovery bypassed Goal registration: %v", err)
	}
	if _, err := restarted.RecoverRun(ctx, runID, nil); err == nil || !strings.Contains(err.Error(), "round binding") {
		t.Fatalf("Run recovery bypassed Goal registration: %v", err)
	}
	if fake.callCount() != 0 {
		t.Fatal("unbound Goal recovery executed a model")
	}
	stored, err := restarted.goalStore().GetGoal(ctx, goal.ID)
	if err != nil || stored.State != agentgoal.StateRunning || stored.Round != 0 {
		t.Fatalf("orphan Goal was guessed or overwritten: %#v %v", stored, err)
	}
}

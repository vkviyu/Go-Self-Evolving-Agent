package conversation

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

type replenishableGoalBudget struct{ exhausted atomic.Bool }

func (budget *replenishableGoalBudget) Budget(context.Context, orchestration.TeamID) (tokenusage.BudgetSnapshot, error) {
	return tokenusage.BudgetSnapshot{Exhausted: budget.exhausted.Load()}, nil
}

func TestFirstGoalBudgetPauseHasDurableBindingAndRecoversOriginalRun(t *testing.T) {
	ctx := context.Background()
	budget := &replenishableGoalBudget{}
	budget.exhausted.Store(true)
	runID, _ := orchestration.NewRunID()
	path := filepath.Join(t.TempDir(), "first-goal-budget.db")
	fake := &goalLoopModel{responses: []*schema.Message{schema.AssistantMessage("accepted after funding", nil)}}
	assessments := 0
	evaluator := agentgoal.EvaluatorFunc(func(context.Context, agentgoal.Evaluation) (agentgoal.Verdict, error) {
		assessments++
		return agentgoal.Verdict{Decision: agentgoal.DecisionComplete, Reason: "verified"}, nil
	})
	chat, database := openRecoveryConversationWithBudget(t, path, fake, evaluator, budget, runID)
	paused, err := chat.RunTurn(ctx, "keep this exact original input", nil)
	if kind, ok := orchestration.ExecutionStopKindOf(err); !ok || kind != orchestration.ExecutionBudget || paused.TurnID == "" || paused.GoalID == "" || paused.GoalRounds != 1 {
		t.Fatalf("pause=%#v err=%v", paused, err)
	}
	if fake.callCount() != 0 || assessments != 0 {
		t.Fatal("budget preflight called model or evaluator")
	}
	discovered, err := DiscoverRecoveries(ctx, chat.ID(), database.SessionEvents(), database.Checkpoints(), database.Orchestration())
	if err != nil || len(discovered) != 1 || discovered[0].Phase != "execution" || discovered[0].TurnID != paused.TurnID || discovered[0].GoalID != paused.GoalID {
		t.Fatalf("unrecoverable first budget pause: %#v err=%v", discovered, err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, reopened := openRecoveryConversationWithBudget(t, path, fake, evaluator, budget, runID)
	if _, err := restarted.RecoverRun(ctx, runID, nil); err == nil {
		t.Fatal("recovery ignored exhausted BudgetReader")
	}
	if fake.callCount() != 0 || assessments != 0 {
		t.Fatal("budget-only recovery spent model calls")
	}
	budget.exhausted.Store(false)
	result, err := restarted.RecoverRun(ctx, runID, nil)
	if err != nil || result.TurnID != paused.TurnID || result.GoalID != paused.GoalID || result.GoalState != agentgoal.StateCompleted || result.GoalRounds != 1 || result.Final == nil || result.Final.Content != "accepted after funding" {
		t.Fatalf("recovery=%#v err=%v", result, err)
	}
	if fake.callCount() != 1 || assessments != 1 {
		t.Fatalf("calls=%d assessments=%d", fake.callCount(), assessments)
	}
	events, err := reopened.SessionEvents().Load(ctx, restarted.ID(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	starts, inputs := 0, 0
	for _, event := range events {
		if event.Type == session.EventTurnStarted {
			starts++
		}
		if event.Type == session.EventUserMessage {
			inputs++
		}
	}
	if starts != 1 || inputs != 1 {
		t.Fatalf("recovery duplicated turn/input: starts=%d inputs=%d", starts, inputs)
	}
}

func TestGoalBudgetPausePreservesCommittedAssessmentAndResumesNextRound(t *testing.T) {
	ctx := context.Background()
	budget := &replenishableGoalBudget{}
	fake := &goalLoopModel{responses: []*schema.Message{
		goalReportCall("continue-report", agentgoal.DecisionContinue, "check remaining acceptance criteria"), schema.AssistantMessage("unaccepted candidate", nil),
		goalReportCall("complete-report", agentgoal.DecisionComplete, "checked"), schema.AssistantMessage("accepted result", nil),
	}}
	assessments := 0
	evaluator := agentgoal.EvaluatorFunc(func(context.Context, agentgoal.Evaluation) (agentgoal.Verdict, error) {
		assessments++
		if assessments == 1 {
			budget.exhausted.Store(true)
			return agentgoal.Verdict{Decision: agentgoal.DecisionContinue, Reason: "incomplete", Feedback: "verify remaining acceptance criterion"}, nil
		}
		return agentgoal.Verdict{Decision: agentgoal.DecisionComplete, Reason: "verified"}, nil
	})
	chat := newGoalConversationWithEvaluator(t, fake, agentgoal.Config{Enabled: true, Activation: agentgoal.ActivationAlways, Assessment: agentgoal.AssessmentIndependent, MaxRounds: 3, MaxWallTime: time.Minute}, budget, evaluator)
	visible := 0
	sink := func(_ context.Context, event agentruntime.Event) error {
		if event.Type == agentruntime.EventAssistantMessage && event.Message != nil && len(event.Message.ToolCalls) == 0 {
			visible++
		}
		return nil
	}
	paused, err := chat.RunTurn(ctx, "complete the task", sink)
	if kind, ok := orchestration.ExecutionStopKindOf(err); !ok || kind != orchestration.ExecutionBudget {
		t.Fatalf("result=%#v err=%v", paused, err)
	}
	goal, round, found, err := chat.goalStore().GoalForTurn(ctx, chat.ID(), paused.TurnID)
	if err != nil || !found || goal.ID != paused.GoalID || goal.State != agentgoal.StateRunning || goal.Round != 1 || round.CompletedAt == nil || round.Feedback != "verify remaining acceptance criterion" || round.Candidate != "unaccepted candidate" {
		t.Fatalf("lost paused assessment: goal=%#v round=%#v found=%v err=%v", goal, round, found, err)
	}
	if visible != 0 || chat.CompletedTurns() != 0 || fake.callCount() != 2 || assessments != 1 {
		t.Fatalf("unaccepted output or duplicate work: visible=%d completed=%d model=%d assessments=%d", visible, chat.CompletedTurns(), fake.callCount(), assessments)
	}
	// Repeated resume while the budget is still unavailable rechecks admission
	// without repeating the worker or committed evaluator decision.
	if _, err := chat.ResumeTurn(ctx, paused.TurnID, sink); err == nil {
		t.Fatal("resume ignored exhausted budget")
	}
	if fake.callCount() != 2 || assessments != 1 {
		t.Fatal("budget retry repeated a completed round")
	}
	budget.exhausted.Store(false)
	result, err := chat.ResumeTurn(ctx, paused.TurnID, sink)
	if err != nil || result.GoalID != goal.ID || result.GoalState != agentgoal.StateCompleted || result.GoalRounds != 2 || result.Final == nil || result.Final.Content != "accepted result" {
		t.Fatalf("resume result=%#v err=%v", result, err)
	}
	if visible != 1 || chat.CompletedTurns() != 1 || fake.callCount() != 4 || assessments != 2 {
		t.Fatalf("resume repeated/lost work: visible=%d completed=%d model=%d assessments=%d", visible, chat.CompletedTurns(), fake.callCount(), assessments)
	}
}

func TestGoalOwnHardCapsRemainTerminalWhenTeamBudgetIsExhausted(t *testing.T) {
	for _, reason := range []agentgoal.StopReason{agentgoal.StopMaxRounds, agentgoal.StopMaxWallTime} {
		t.Run(string(reason), func(t *testing.T) {
			chat := newGoalConversation(t, &goalLoopModel{}, agentgoal.Config{Enabled: true, Activation: agentgoal.ActivationAlways, Assessment: agentgoal.AssessmentSelf, MaxRounds: 3, MaxWallTime: time.Minute}, exhaustedBudget{})
			scope := chat.factory.ActorScope()
			goal, err := chat.goalStore().CreateGoal(context.Background(), agentgoal.CreateRequest{ID: "hard-cap", SessionID: chat.ID(), TeamID: scope.TeamID, AgentID: scope.AgentID, Text: "work", Assessment: agentgoal.AssessmentSelf, MaxRounds: 3, MaxWallTime: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			result, err := chat.stopGoalCap(context.Background(), goal, agentruntime.Result{}, reason, &candidateEventBuffer{})
			if err != nil || result.GoalState != agentgoal.StateExhausted || result.GoalStopReason != reason {
				t.Fatalf("hard cap changed: %#v %v", result, err)
			}
		})
	}
}

package conversation

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

func TestRecoveryDiscoveryReportsDurableGoalPhaseWithoutExecution(t *testing.T) {
	for _, phase := range []string{"assessment", "verdict", "unstarted-next-round", "terminal"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			fake := &goalLoopModel{responses: []*schema.Message{schema.AssistantMessage("candidate", nil)}}
			evaluator := agentgoal.EvaluatorFunc(func(context.Context, agentgoal.Evaluation) (agentgoal.Verdict, error) {
				t.Fatal("discovery invoked model")
				return agentgoal.Verdict{}, nil
			})
			chat, _ := openRecoveryConversation(t, filepath.Join(t.TempDir(), "goal.db"), fake, evaluator)
			goal, round, result := runRecoveryCandidate(t, chat)
			if phase != "assessment" {
				if _, err := chat.goalStore().FinishRound(ctx, goal.ID, round.Number, agentgoal.Verdict{Decision: agentgoal.DecisionContinue, Reason: "more work", Feedback: "next"}, result.Final.Content); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "unstarted-next-round" {
				if _, err := chat.goalStore().StartRound(ctx, goal.ID, 2, "next-turn", "next"); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "terminal" {
				if _, err := chat.goalStore().FinishGoal(ctx, goal.ID, agentgoal.Finish{State: agentgoal.StateExhausted, Decision: agentgoal.DecisionContinue, StopReason: agentgoal.StopMaxRounds, Reason: "limit"}); err != nil {
					t.Fatal(err)
				}
			}
			items, err := chat.Recoveries(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if phase == "terminal" {
				if len(items) != 0 {
					t.Fatalf("terminal listed=%#v", items)
				}
				return
			}
			wantPhase, wantTurn := phase, result.TurnID
			if phase == "unstarted-next-round" {
				wantPhase, wantTurn = "unavailable", "next-turn"
			}
			if len(items) != 1 || items[0].Phase != wantPhase || items[0].TurnID != wantTurn || items[0].GoalID != goal.ID {
				t.Fatalf("discovery=%#v", items)
			}
			if fake.callCount() != 1 {
				t.Fatal("discovery repeated execution")
			}
		})
	}
}

func TestRecoveryDiscoveryDoesNotHideOrphanGoalOrMissingCheckpoint(t *testing.T) {
	ctx := context.Background()
	fake := &goalLoopModel{}
	chat, database := openRecoveryConversation(t, ":memory:", fake, nil)
	scope := chat.factory.ActorScope()
	_, err := chat.goalStore().CreateGoal(ctx, agentgoal.CreateRequest{ID: "orphan", SessionID: chat.ID(), TeamID: scope.TeamID, AgentID: scope.AgentID, Text: "interrupted before round", Assessment: agentgoal.AssessmentSelf, MaxRounds: 3, MaxWallTime: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.SessionEvents().Append(ctx, chat.ID(), session.AnySequence, session.PendingEvent{Type: session.EventTurnStarted, CorrelationID: "missing-checkpoint", Payload: session.MustJSONPayload(map[string]any{})})
	if err != nil {
		t.Fatal(err)
	}
	items, err := chat.Recoveries(ctx)
	if err != nil || len(items) != 2 {
		t.Fatalf("discovery=%#v %v", items, err)
	}
	for _, item := range items {
		if item.Phase != "unavailable" || item.Reason == "" {
			t.Fatalf("unsafe candidate=%#v", item)
		}
	}
	if fake.callCount() != 0 {
		t.Fatal("discovery started work")
	}
}

func TestRecoverRunFindsAssessmentAndReusesTerminalGoalResult(t *testing.T) {
	ctx := context.Background()
	runID, _ := orchestration.NewRunID()
	path := filepath.Join(t.TempDir(), "goal.db")
	fake := &goalLoopModel{responses: []*schema.Message{schema.AssistantMessage("durable candidate", nil)}}
	assessments := 0
	evaluator := agentgoal.EvaluatorFunc(func(context.Context, agentgoal.Evaluation) (agentgoal.Verdict, error) {
		assessments++
		return agentgoal.Verdict{Decision: agentgoal.DecisionComplete, Reason: "verified"}, nil
	})
	chat, database := openRecoveryConversation(t, path, fake, evaluator, runID)
	goal, _, original := runRecoveryCandidate(t, chat)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, reopened := openRecoveryConversation(t, path, fake, evaluator, runID)
	result, err := restarted.RecoverRun(ctx, runID, nil)
	if err != nil || result.GoalState != agentgoal.StateCompleted || result.TurnID != original.TurnID {
		t.Fatalf("assessment recovery=%#v %v", result, err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, _ = openRecoveryConversation(t, path, fake, evaluator, runID)
	result, err = restarted.RecoverRun(ctx, runID, nil)
	if err != nil || result.GoalID != goal.ID || result.Final.Content != "durable candidate" || result.GoalState != agentgoal.StateCompleted {
		t.Fatalf("terminal recovery=%#v %v", result, err)
	}
	if fake.callCount() != 1 || assessments != 1 || restarted.CompletedTurns() != 1 {
		t.Fatalf("duplicate work: calls=%d assessments=%d", fake.callCount(), assessments)
	}
}

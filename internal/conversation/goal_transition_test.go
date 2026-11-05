package conversation

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

func TestLiveAndRecoveredVerdictsShareTerminalSemantics(t *testing.T) {
	for _, decision := range []agentgoal.Decision{agentgoal.DecisionComplete, agentgoal.DecisionBlocked, agentgoal.DecisionContinue} {
		for _, recovered := range []bool{false, true} {
			name := string(decision) + "/live"
			if recovered {
				name = string(decision) + "/recovered"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				fake := &goalLoopModel{responses: []*schema.Message{schema.AssistantMessage("candidate", nil)}}
				assessments := 0
				verdict := agentgoal.Verdict{Decision: decision, Reason: "reviewed evidence", Feedback: "specific missing check"}
				evaluator := agentgoal.EvaluatorFunc(func(context.Context, agentgoal.Evaluation) (agentgoal.Verdict, error) {
					assessments++
					return verdict, nil
				})
				path := filepath.Join(t.TempDir(), "goal.db")
				chat, db := openRecoveryConversation(t, path, fake, evaluator)
				goal, round, candidate := runRecoveryCandidateWithCaps(t, chat, 1, time.Now())
				if recovered {
					if _, err := chat.goalStore().FinishRound(ctx, goal.ID, round.Number, verdict, candidate.Final.Content); err != nil {
						t.Fatal(err)
					}
					if err := db.Close(); err != nil {
						t.Fatal(err)
					}
					chat, _ = openRecoveryConversation(t, path, fake, evaluator)
				}
				visible := 0
				var phases []string
				sink := func(ctx context.Context, event agentruntime.Event) error {
					if event.Type == agentruntime.EventAssistantMessage {
						visible++
						stored, err := chat.goalStore().GetGoal(ctx, goal.ID)
						if err != nil || stored.State == agentgoal.StateRunning {
							t.Errorf("candidate exposed before durable terminal state: %+v %v", stored, err)
						}
					}
					if event.GoalPhase != nil {
						phases = append(phases, event.GoalPhase.Stage)
					}
					return nil
				}
				var result agentruntime.Result
				var err error
				if recovered {
					result, err = chat.ResumeTurn(ctx, round.TurnID, sink)
				} else {
					chat.mutex.Lock()
					result, err = chat.runGoal(ctx, goal, round, "", "", evaluator, agentgoal.CapGuard{}, sink, &completedGoalRound{result: candidate, buffer: restoredCandidateBuffer(candidate, sink)})
					chat.mutex.Unlock()
				}
				wantState, wantStop := agentgoal.StateCompleted, agentgoal.StopSemanticComplete
				if decision == agentgoal.DecisionBlocked {
					wantState, wantStop = agentgoal.StateBlocked, agentgoal.StopSemanticBlocked
				}
				if decision == agentgoal.DecisionContinue {
					wantState, wantStop = agentgoal.StateExhausted, agentgoal.StopMaxRounds
				}
				if err != nil || result.GoalState != wantState || result.GoalStopReason != wantStop || result.GoalDecision != decision || result.GoalRounds != 1 || result.Final == nil || result.Final.Content != "candidate" {
					t.Fatalf("result=%+v err=%v", result, err)
				}
				wantAssessments, wantPhases := 1, []string{"verifying", "stopped"}
				if recovered {
					wantAssessments, wantPhases = 0, []string{"stopped"}
				}
				if assessments != wantAssessments || fake.callCount() != 1 || visible != 1 || chat.CompletedTurns() != 1 || !reflect.DeepEqual(phases, wantPhases) {
					t.Fatalf("assessments=%d model=%d visible=%d completed=%d phases=%v", assessments, fake.callCount(), visible, chat.CompletedTurns(), phases)
				}
			})
		}
	}
}

func TestCommittedTerminalVerdictSurvivesExpiredExecutionDeadline(t *testing.T) {
	for _, decision := range []agentgoal.Decision{agentgoal.DecisionComplete, agentgoal.DecisionBlocked} {
		t.Run(string(decision), func(t *testing.T) {
			ctx := context.Background()
			fake := &goalLoopModel{responses: []*schema.Message{schema.AssistantMessage("durable candidate", nil)}}
			evaluator := agentgoal.EvaluatorFunc(func(context.Context, agentgoal.Evaluation) (agentgoal.Verdict, error) {
				t.Error("committed verdict was reevaluated")
				return agentgoal.Verdict{}, errors.New("unexpected evaluation")
			})
			chat, _ := openRecoveryConversation(t, filepath.Join(t.TempDir(), "goal.db"), fake, evaluator)
			goal, round, result := runRecoveryCandidateWithCaps(t, chat, 3, time.Now().Add(-time.Hour))
			if _, err := chat.goalStore().FinishRound(ctx, goal.ID, round.Number, agentgoal.Verdict{Decision: decision, Reason: "already reviewed"}, result.Final.Content); err != nil {
				t.Fatal(err)
			}
			result, err := chat.ResumeTurn(ctx, round.TurnID, nil)
			if err != nil || result.GoalDecision != decision || result.GoalState == agentgoal.StateExhausted || result.Final.Content != "durable candidate" || fake.callCount() != 1 {
				t.Fatalf("result=%+v calls=%d err=%v", result, fake.callCount(), err)
			}
		})
	}
}

type transitionBudgetFailure struct{ err error }

func (budget transitionBudgetFailure) Budget(context.Context, orchestration.TeamID) (tokenusage.BudgetSnapshot, error) {
	return tokenusage.BudgetSnapshot{}, budget.err
}

func TestGoalContinuationDoesNotDispatchAndPreservesGuardFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		ctx := context.Background()
		fake := &goalLoopModel{responses: []*schema.Message{schema.AssistantMessage("candidate", nil)}}
		chat, _ := openRecoveryConversation(t, filepath.Join(t.TempDir(), "goal.db"), fake, nil)
		goal, round, candidate := runRecoveryCandidate(t, chat)
		verdict := agentgoal.Verdict{Decision: agentgoal.DecisionContinue, Reason: "incomplete", Feedback: "preserved feedback"}
		if _, err := chat.goalStore().FinishRound(ctx, goal.ID, round.Number, verdict, candidate.Final.Content); err != nil {
			t.Fatal(err)
		}
		goal, err := chat.goalStore().GetGoal(ctx, goal.ID)
		if err != nil {
			t.Fatal(err)
		}
		guard := agentgoal.CapGuard{}
		failure := errors.New("budget read failed")
		if fail {
			guard.Budget = transitionBudgetFailure{failure}
		}
		visible := 0
		buffer := restoredCandidateBuffer(candidate, func(context.Context, agentruntime.Event) error { visible++; return nil })
		chat.mutex.Lock()
		transition, err := chat.applyGoalVerdict(ctx, goal, verdict, candidate, buffer, guard)
		chat.mutex.Unlock()
		if fake.callCount() != 1 || visible != 0 || chat.CompletedTurns() != 0 {
			t.Fatal("transition executed a worker or exposed a rejected candidate")
		}
		if fail {
			if !errors.Is(err, failure) || transition.continueRound {
				t.Fatalf("transition=%+v err=%v", transition, err)
			}
			stored, err := chat.goalStore().GetGoal(ctx, goal.ID)
			if err != nil || stored.State != agentgoal.StateFailed {
				t.Fatalf("failure not durable: %+v %v", stored, err)
			}
		} else if err != nil || !transition.continueRound || !strings.Contains(transition.nextInput, verdict.Feedback) || transition.result.TurnID != candidate.TurnID {
			t.Fatalf("transition=%+v err=%v", transition, err)
		}
	}
}

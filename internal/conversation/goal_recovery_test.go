package conversation

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/sqlitestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func openRecoveryConversation(t *testing.T, path string, fake *goalLoopModel, evaluator agentgoal.Evaluator, runIDs ...orchestration.RunID) (*Conversation, *sqlitestore.Database) {
	t.Helper()
	return openRecoveryConversationWithBudget(t, path, fake, evaluator, nil, runIDs...)
}

func openRecoveryConversationWithBudget(t *testing.T, path string, fake *goalLoopModel, evaluator agentgoal.Evaluator, budget agentgoal.BudgetReader, runIDs ...orchestration.RunID) (*Conversation, *sqlitestore.Database) {
	t.Helper()
	ctx := context.Background()
	database, err := sqlitestore.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	root, _, err := database.Orchestration().EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "recover-goal"})
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry()
	if _, err := registry.Activate(agentgoal.Binding(database.Orchestration())); err != nil {
		t.Fatal(err)
	}
	engine, err := policy.DefaultDeny(policy.Rule{ID: "report", ToolPattern: agentgoal.ReportToolName, CapabilityPattern: "*", Effect: policy.EffectAllow})
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := tool.NewPipeline(tool.PipelineConfig{Registry: registry, Policy: engine, Events: database.SessionEvents()})
	if err != nil {
		t.Fatal(err)
	}
	scope := root.ActorScope()
	if len(runIDs) > 0 {
		scope.RunID = runIDs[0]
	}
	factory, err := agentruntime.NewFactory(agentruntime.Config{Instruction: "Execute only authorized work.", ActorScope: scope, GoalMode: agentgoal.Config{Enabled: true, Activation: agentgoal.ActivationAlways, Assessment: agentgoal.AssessmentIndependent, MaxRounds: 3, MaxWallTime: time.Minute}, GoalStore: database.Orchestration(), GoalEvaluator: evaluator, GoalBudget: budget}, fake, registry, pipeline, database.SessionEvents(), database.Checkpoints())
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := Open(ctx, factory, root.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	return conversation, database
}

func runRecoveryCandidate(t *testing.T, conversation *Conversation) (agentgoal.Goal, agentgoal.Round, agentruntime.Result) {
	t.Helper()
	return runRecoveryCandidateWithCaps(t, conversation, 3, time.Now())
}

func runRecoveryCandidateWithCaps(t *testing.T, conversation *Conversation, maxRounds int, startedAt time.Time) (agentgoal.Goal, agentgoal.Round, agentruntime.Result) {
	t.Helper()
	ctx := context.Background()
	actor := conversation.factory.ActorScope()
	goal, err := conversation.goalStore().CreateGoal(ctx, agentgoal.CreateRequest{ID: "goal-recovery", SessionID: conversation.sessionID, TeamID: actor.TeamID, AgentID: actor.AgentID, Text: "finish the task", Assessment: agentgoal.AssessmentIndependent, MaxRounds: maxRounds, MaxWallTime: time.Minute, StartedAt: startedAt})
	if err != nil {
		t.Fatal(err)
	}
	result, err := conversation.runInnerTurn(ctx, goal.Text, agentruntime.ContextHints{GoalID: goal.ID, GoalText: goal.Text, GoalRound: 1}, nil, func(id string) error {
		_, err := conversation.goalStore().StartRound(ctx, goal.ID, 1, id, goal.Text)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	goal, round, found, err := conversation.goalStore().GoalForTurn(ctx, conversation.sessionID, result.TurnID)
	if err != nil || !found {
		t.Fatalf("goal lookup: %v %v", found, err)
	}
	return goal, round, result
}

func TestGoalRecoveryRetainsCandidateWhenContinuationExhausted(t *testing.T) {
	for _, limit := range []agentgoal.StopReason{agentgoal.StopMaxRounds, agentgoal.StopMaxWallTime} {
		t.Run(string(limit), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "goal.db")
			fake := &goalLoopModel{responses: []*schema.Message{schema.AssistantMessage("partial result", nil)}}
			evaluator := agentgoal.EvaluatorFunc(func(context.Context, agentgoal.Evaluation) (agentgoal.Verdict, error) {
				t.Fatal("exhausted recovery invoked evaluator")
				return agentgoal.Verdict{}, nil
			})
			conversation, database := openRecoveryConversation(t, path, fake, evaluator)
			maxRounds, startedAt := 1, time.Now()
			if limit == agentgoal.StopMaxWallTime {
				maxRounds, startedAt = 3, time.Now().Add(-time.Hour)
			}
			goal, round, result := runRecoveryCandidateWithCaps(t, conversation, maxRounds, startedAt)
			if _, err := conversation.goalStore().FinishRound(context.Background(), goal.ID, round.Number, agentgoal.Verdict{Decision: agentgoal.DecisionContinue, Reason: "unfinished"}, result.Final.Content); err != nil {
				t.Fatal(err)
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}
			restarted, _ := openRecoveryConversation(t, path, fake, evaluator)
			resumed, err := restarted.ResumeTurn(context.Background(), result.TurnID, nil)
			if err != nil || resumed.GoalState != agentgoal.StateExhausted || resumed.GoalStopReason != limit || resumed.Final == nil || resumed.Final.Content != "partial result" || fake.callCount() != 1 {
				t.Fatalf("exhausted resume=%#v calls=%d err=%v", resumed, fake.callCount(), err)
			}
			stored, err := restarted.goalStore().GetGoal(context.Background(), goal.ID)
			if err != nil || stored.Result != "partial result" {
				t.Fatalf("lost partial result=%#v %v", stored, err)
			}
		})
	}
}

func TestGoalRecoveryAfterExecutionOnlyRunsAssessment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "goal.db")
	fake := &goalLoopModel{responses: []*schema.Message{schema.AssistantMessage("durable candidate", nil)}}
	assessments := 0
	evaluator := agentgoal.EvaluatorFunc(func(_ context.Context, input agentgoal.Evaluation) (agentgoal.Verdict, error) {
		assessments++
		if input.Candidate != "durable candidate" || input.Round.Number != 1 {
			t.Fatalf("lost candidate: %#v", input)
		}
		return agentgoal.Verdict{Decision: agentgoal.DecisionComplete, Reason: "verified"}, nil
	})
	conversation, database := openRecoveryConversation(t, path, fake, evaluator)
	_, _, result := runRecoveryCandidate(t, conversation)
	// Inner Loop has already deleted its Eino checkpoint.
	if _, err := conversation.factory.ResumeTurn(context.Background(), conversation.sessionID, result.TurnID); err == nil {
		t.Fatal("expected completed checkpoint to be gone")
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, _ := openRecoveryConversation(t, path, fake, evaluator)
	visible := 0
	resumed, err := restarted.ResumeTurn(context.Background(), result.TurnID, func(_ context.Context, event agentruntime.Event) error {
		if event.Type == agentruntime.EventAssistantMessage {
			visible++
		}
		return nil
	})
	if err != nil || resumed.GoalState != agentgoal.StateCompleted || resumed.Final.Content != "durable candidate" {
		t.Fatalf("resume=%#v %v", resumed, err)
	}
	if fake.callCount() != 1 || assessments != 1 || visible != 1 || restarted.CompletedTurns() != 1 {
		t.Fatalf("worker=%d assess=%d visible=%d turns=%d", fake.callCount(), assessments, visible, restarted.CompletedTurns())
	}
}

func TestGoalRecoveryUsesCommittedVerdictWithoutWorkerOrAssessment(t *testing.T) {
	for _, decision := range []agentgoal.Decision{agentgoal.DecisionComplete, agentgoal.DecisionBlocked} {
		t.Run(string(decision), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "goal.db")
			fake := &goalLoopModel{responses: []*schema.Message{schema.AssistantMessage("accepted or blocked candidate", nil)}}
			evaluator := agentgoal.EvaluatorFunc(func(context.Context, agentgoal.Evaluation) (agentgoal.Verdict, error) {
				t.Fatal("repeated persisted assessment")
				return agentgoal.Verdict{}, nil
			})
			conversation, database := openRecoveryConversation(t, path, fake, evaluator)
			goal, round, result := runRecoveryCandidate(t, conversation)
			_, err := conversation.goalStore().FinishRound(context.Background(), goal.ID, round.Number, agentgoal.Verdict{Decision: decision, Reason: "durable decision", Feedback: "durable feedback"}, result.Final.Content)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}
			restarted, _ := openRecoveryConversation(t, path, fake, evaluator)
			resumed, err := restarted.ResumeTurn(context.Background(), result.TurnID, nil)
			if err != nil || resumed.GoalDecision != decision || fake.callCount() != 1 {
				t.Fatalf("resume=%#v calls=%d err=%v", resumed, fake.callCount(), err)
			}
		})
	}
}

func TestGoalRecoveryContinuesOnlyAfterCommittedRejection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "goal.db")
	fake := &goalLoopModel{responses: []*schema.Message{schema.AssistantMessage("first candidate", nil), schema.AssistantMessage("corrected candidate", nil)}}
	assessments := 0
	evaluator := agentgoal.EvaluatorFunc(func(_ context.Context, input agentgoal.Evaluation) (agentgoal.Verdict, error) {
		assessments++
		if input.Round.Number != 2 || !strings.Contains(input.Round.Input, "specific persisted feedback") {
			t.Fatalf("wrong resumed round: %#v", input.Round)
		}
		return agentgoal.Verdict{Decision: agentgoal.DecisionComplete, Reason: "fixed"}, nil
	})
	conversation, database := openRecoveryConversation(t, path, fake, evaluator)
	goal, round, result := runRecoveryCandidate(t, conversation)
	_, err := conversation.goalStore().FinishRound(context.Background(), goal.ID, round.Number, agentgoal.Verdict{Decision: agentgoal.DecisionContinue, Reason: "not accepted", Feedback: "specific persisted feedback"}, result.Final.Content)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, _ := openRecoveryConversation(t, path, fake, evaluator)
	resumed, err := restarted.ResumeTurn(context.Background(), result.TurnID, nil)
	if err != nil || resumed.GoalState != agentgoal.StateCompleted || resumed.GoalRounds != 2 || fake.callCount() != 2 || assessments != 1 {
		t.Fatalf("resume=%#v calls=%d assess=%d err=%v", resumed, fake.callCount(), assessments, err)
	}
}

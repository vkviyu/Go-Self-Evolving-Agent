package conversation

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

func TestAbandonedGoalCannotResumeAssessmentOrCommittedVerdict(t *testing.T) {
	for _, verdictSaved := range []bool{false, true} {
		t.Run(map[bool]string{false: "assessment", true: "verdict"}[verdictSaved], func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "goal.db")
			fake := &goalLoopModel{responses: []*schema.Message{schema.AssistantMessage("candidate", nil)}}
			evaluator := agentgoal.EvaluatorFunc(func(context.Context, agentgoal.Evaluation) (agentgoal.Verdict, error) {
				t.Fatal("abandoned Goal reached evaluator")
				return agentgoal.Verdict{}, nil
			})
			runID, err := orchestration.NewRunID()
			if err != nil {
				t.Fatal(err)
			}
			chat, database := openRecoveryConversation(t, path, fake, evaluator, runID)
			goal, round, result := runRecoveryCandidate(t, chat)
			if verdictSaved {
				if _, err := database.Orchestration().FinishRound(ctx, goal.ID, round.Number, agentgoal.Verdict{Decision: agentgoal.DecisionContinue, Reason: "continue later"}, result.Final.Content); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := database.AbandonRecovery(ctx, session.AbandonRequest{SessionID: chat.ID(), TurnID: result.TurnID, Reason: "effects reviewed", EffectsReviewed: true}); err != nil {
				t.Fatal(err)
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}
			restarted, reopened := openRecoveryConversation(t, path, fake, evaluator, runID)
			if items, err := restarted.Recoveries(ctx); err != nil || len(items) != 0 {
				t.Fatalf("abandoned Goal offered for recovery: %+v %v", items, err)
			}
			if items, err := DiscoverRecoveries(ctx, restarted.ID(), reopened.SessionEvents(), reopened.Checkpoints(), reopened.Orchestration()); err != nil || len(items) != 0 {
				t.Fatalf("offline discovery offered abandoned Goal: %+v %v", items, err)
			}
			if _, err := restarted.ResumeTurn(ctx, result.TurnID, nil); !errors.Is(err, session.ErrRecoveryAbandoned) {
				t.Fatalf("explicit resume accepted: %v", err)
			}
			if _, err := restarted.RecoverRun(ctx, runID, nil); !errors.Is(err, session.ErrRecoveryAbandoned) {
				t.Fatalf("run recovery accepted: %v", err)
			}
			if fake.callCount() != 1 {
				t.Fatal("abandonment replayed worker")
			}
		})
	}
}

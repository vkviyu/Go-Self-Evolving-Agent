package conversation

import (
	"context"
	"errors"
	"fmt"

	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
)

func (conversation *Conversation) resumeGoal(ctx context.Context, goal agentgoal.Goal, round agentgoal.Round, evaluator agentgoal.Evaluator, guard agentgoal.CapGuard, sink agentruntime.EventSink) (agentruntime.Result, error) {
	if round.Number != goal.Round {
		return agentruntime.Result{}, fmt.Errorf("goal is already on round %d; refusing stale round %d recovery", goal.Round, round.Number)
	}
	if round.CompletedAt != nil {
		// Assessment is already committed. Reuse its decision and feedback;
		// calling either the worker or evaluator here would duplicate work.
		verdict := agentgoal.Verdict{Decision: round.Decision, Reason: round.Reason, Feedback: round.Feedback}
		if err := verdict.Validate(); err != nil {
			return agentruntime.Result{}, err
		}
		if round.Candidate == "" {
			return agentruntime.Result{}, errors.New("assessed round has no candidate")
		}
		result := agentruntime.Result{TurnID: round.TurnID, Final: schema.AssistantMessage(round.Candidate, nil)}
		buffer := restoredCandidateBuffer(result, sink)
		if verdict.Decision == agentgoal.DecisionContinue {
			ctxWithDeadline, cancel := context.WithDeadlineCause(ctx, goal.StartedAt.Add(goal.MaxWallTime), errGoalDeadline)
			defer cancel()
			ctx = ctxWithDeadline
			if ctx.Err() != nil {
				return conversation.stopGoalError(ctx, goal, result, buffer, ctx.Err())
			}
		} else {
			// No new work: only commit an already-durable terminal decision,
			// even if execution's deadline elapsed during the process outage.
			durable, cancel := goalDurableContext(ctx)
			defer cancel()
			ctx = durable
		}
		transition, err := conversation.applyGoalVerdict(ctx, goal, verdict, result, buffer, guard)
		if err != nil || !transition.continueRound {
			return transition.result, err
		}
		return conversation.runGoal(ctx, goal, round, transition.nextInput, "", evaluator, guard, sink, nil)
	}
	result, completed, err := conversation.factory.CompletedTurn(ctx, conversation.sessionID, round.TurnID)
	if err != nil {
		return agentruntime.Result{}, err
	}
	if completed {
		return conversation.runGoal(ctx, goal, round, "", "", evaluator, guard, sink, &completedGoalRound{result: result, buffer: restoredCandidateBuffer(result, sink)})
	}
	return conversation.runGoal(ctx, goal, round, "", round.TurnID, evaluator, guard, sink, nil)
}

func restoredCandidateBuffer(result agentruntime.Result, sink agentruntime.EventSink) *candidateEventBuffer {
	return &candidateEventBuffer{downstream: sink, events: []agentruntime.Event{{Type: agentruntime.EventAssistantMessage, TurnID: result.TurnID, Message: result.Final}}}
}

package conversation

import (
	"context"
	"fmt"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
)

// goalTransition is the result of applying an already-validated, durable
// verdict. Only a permitted continuation may schedule another inner Turn.
type goalTransition struct {
	result        agentruntime.Result
	continueRound bool
	nextInput     string
}

// applyGoalVerdict is shared by live assessment and committed-verdict recovery.
// It never executes a worker or evaluator. Callers choose the appropriate
// deadline: ongoing work uses the original Goal deadline; recovery of a
// terminal verdict uses a bounded durable context even after that deadline.
func (conversation *Conversation) applyGoalVerdict(ctx context.Context, goal agentgoal.Goal, verdict agentgoal.Verdict, result agentruntime.Result, buffer *candidateEventBuffer, guard agentgoal.CapGuard) (goalTransition, error) {
	switch verdict.Decision {
	case agentgoal.DecisionComplete, agentgoal.DecisionBlocked:
		state, reason := agentgoal.StateCompleted, agentgoal.StopSemanticComplete
		if verdict.Decision == agentgoal.DecisionBlocked {
			state, reason = agentgoal.StateBlocked, agentgoal.StopSemanticBlocked
		}
		candidate := ""
		if result.Final != nil {
			candidate = result.Final.Content
		}
		finished, err := conversation.finishGoal(ctx, goal, result, agentgoal.Finish{
			State: state, Decision: verdict.Decision, StopReason: reason,
			Reason: verdict.Reason, Result: candidate,
		}, buffer)
		return goalTransition{result: finished}, err
	case agentgoal.DecisionContinue:
		if reason, reached, err := guard.Reached(ctx, goal, goal.Round+1); err != nil {
			stopped, err := conversation.stopGoalError(ctx, goal, result, buffer, err)
			return goalTransition{result: stopped}, err
		} else if reached {
			stopped, err := conversation.stopGoalCap(ctx, goal, result, reason, buffer)
			return goalTransition{result: stopped}, err
		}
		return goalTransition{result: result, continueRound: true, nextInput: continuationPrompt(goal, verdict)}, nil
	default:
		return goalTransition{result: result}, fmt.Errorf("invalid goal decision %q", verdict.Decision)
	}
}

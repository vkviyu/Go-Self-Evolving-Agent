package conversation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

func (conversation *Conversation) finishGoal(ctx context.Context, goal agentgoal.Goal, result agentruntime.Result, finish agentgoal.Finish, buffer *candidateEventBuffer) (agentruntime.Result, error) {
	completed, err := conversation.goalStore().FinishGoal(ctx, goal.ID, finish)
	if err != nil {
		return conversation.stopGoalError(ctx, goal, result, buffer, err)
	}
	result.GoalID, result.GoalState, result.GoalDecision = completed.ID, completed.State, completed.Decision
	result.GoalStopReason, result.GoalRounds = completed.StopReason, completed.Round
	result.GoalReason = completed.Reason
	if repository, ok := conversation.goalStore().(agentgoal.AdmissionRepository); ok {
		for _, state := range []agentgoal.AdmissionState{agentgoal.AdmissionRoundStarted, agentgoal.AdmissionTurnStarted} {
			admissions, listErr := repository.List(ctx, state)
			if listErr != nil {
				return result, listErr
			}
			for _, admission := range admissions {
				if admission.GoalID != goal.ID {
					continue
				}
				if _, consumeErr := repository.Consume(ctx, admission.Token, admission.Version); consumeErr != nil {
					return result, fmt.Errorf("consume Goal admission: %w", consumeErr)
				}
				break
			}
		}
	}
	if err := buffer.Flush(ctx); err != nil {
		return result, err
	}
	conversation.completedTurns++
	if err := goalPhase(ctx, buffer.downstream, completed, completed.Round, "stopped", completed.Reason); err != nil {
		return result, err
	}
	return result, nil
}

func (conversation *Conversation) exhaustGoal(ctx context.Context, goal agentgoal.Goal, result agentruntime.Result, reason agentgoal.StopReason, buffer *candidateEventBuffer) (agentruntime.Result, error) {
	message := "Goal Mode hard limit reached: " + string(reason)
	finish := agentgoal.Finish{State: agentgoal.StateExhausted, Decision: agentgoal.DecisionContinue, StopReason: reason, Reason: message}
	if result.Final != nil {
		finish.Result = result.Final.Content
	} else {
		result.Final = schema.AssistantMessage(message, nil)
	}
	return conversation.finishGoal(ctx, goal, result, finish, buffer)
}

// A Team budget is external admission state that can be replenished. Preserve
// the Goal/round/candidate for recovery; only the Goal's own hard caps end it.
func (conversation *Conversation) stopGoalCap(ctx context.Context, goal agentgoal.Goal, result agentruntime.Result, reason agentgoal.StopReason, buffer *candidateEventBuffer) (agentruntime.Result, error) {
	if reason == agentgoal.StopBudgetExhausted {
		return conversation.stopGoalError(ctx, goal, result, buffer, goalBudgetPauseError())
	}
	return conversation.exhaustGoal(ctx, goal, result, reason, buffer)
}

func goalBudgetPauseError() error {
	return &orchestration.ExecutionStop{Kind: orchestration.ExecutionBudget, Cause: errors.New("Team usage budget is exhausted")}
}

func (conversation *Conversation) failGoal(ctx context.Context, goal agentgoal.Goal, cause error) error {
	state, reason := agentgoal.StateFailed, agentgoal.StopFailure
	if errors.Is(cause, context.Canceled) {
		state, reason = agentgoal.StateCanceled, agentgoal.StopCanceled
	}
	durable, cancel := goalDurableContext(ctx)
	defer cancel()
	_, finishErr := conversation.goalStore().FinishGoal(durable, goal.ID, agentgoal.Finish{State: state, StopReason: reason, Reason: cause.Error()})
	return errors.Join(cause, finishErr)
}

// goalDurableContext bounds a Goal store write that must outlive the
// cancellation or deadline of the goal it records.
func goalDurableContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}

func goalPhase(ctx context.Context, sink agentruntime.EventSink, goal agentgoal.Goal, round int, stage, reason string) error {
	if sink == nil {
		return nil
	}
	return sink(ctx, agentruntime.Event{Type: agentruntime.EventGoalPhase, GoalPhase: &agentruntime.GoalPhase{
		Stage: stage, GoalID: goal.ID, Round: round, MaxRounds: goal.MaxRounds, State: string(goal.State), Reason: reason,
	}})
}

func (conversation *Conversation) stopGoalError(ctx context.Context, goal agentgoal.Goal, result agentruntime.Result, buffer *candidateEventBuffer, cause error) (agentruntime.Result, error) {
	if _, paused := orchestration.ExecutionStopKindOf(cause); paused {
		result.GoalID, result.GoalState, result.GoalDecision = goal.ID, goal.State, goal.Decision
		result.GoalStopReason, result.GoalRounds = goal.StopReason, goal.Round
		result.GoalReason = cause.Error()
		durable, cancel := goalDurableContext(ctx)
		defer cancel()
		_ = goalPhase(durable, buffer.downstream, goal, goal.Round, "paused", cause.Error())
		return result, cause
	}
	if errors.Is(context.Cause(ctx), errGoalDeadline) {
		durable, cancel := goalDurableContext(ctx)
		defer cancel()
		return conversation.exhaustGoal(durable, goal, result, agentgoal.StopMaxWallTime, buffer)
	}
	if ctx.Err() != nil {
		cause = errors.Join(cause, ctx.Err())
	}
	return result, conversation.failGoal(ctx, goal, cause)
}

package conversation

import (
	"context"
	"errors"
	"fmt"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

func (conversation *Conversation) runGoal(
	ctx context.Context,
	goal agentgoal.Goal,
	round agentgoal.Round,
	input, resumeTurnID string,
	evaluator agentgoal.Evaluator,
	guard agentgoal.CapGuard,
	sink agentruntime.EventSink,
	first *completedGoalRound,
) (agentruntime.Result, error) {
	if evaluator == nil {
		return agentruntime.Result{}, conversation.failGoal(ctx, goal, errors.New("Goal Mode evaluator is unavailable"))
	}
	var last agentruntime.Result
	ctx, cancel := context.WithDeadlineCause(ctx, goal.StartedAt.Add(goal.MaxWallTime), errGoalDeadline)
	defer cancel()
	for {
		if ctx.Err() != nil {
			buffer := &candidateEventBuffer{downstream: sink}
			if first != nil {
				last, buffer = first.result, first.buffer
			}
			return conversation.stopGoalError(ctx, goal, last, buffer, ctx.Err())
		}
		nextRound := goal.Round + 1
		if first != nil || resumeTurnID != "" {
			nextRound = round.Number
		}
		preflight := guard
		if goal.Round == 0 {
			// Persist the first turn's exact input and round binding before a
			// budget pause. Recovery can then resume that turn without inventing
			// a replacement run. The admission callback still gates the model.
			preflight.Budget = nil
		}
		if reason, reached, err := preflight.Reached(ctx, goal, nextRound); err != nil {
			return conversation.stopGoalError(ctx, goal, last, &candidateEventBuffer{downstream: sink}, err)
		} else if reached {
			if first != nil {
				last = first.result
			}
			return conversation.stopGoalCap(ctx, goal, last, reason, &candidateEventBuffer{downstream: sink})
		}

		buffer := &candidateEventBuffer{downstream: sink}
		var err error
		if first != nil {
			last, buffer = first.result, first.buffer
			first = nil
		} else if phaseErr := goalPhase(ctx, sink, goal, nextRound, "executing", ""); phaseErr != nil {
			return conversation.stopGoalError(ctx, goal, last, buffer, phaseErr)
		} else if resumeTurnID != "" {
			last, err = conversation.resumeInnerTurn(ctx, resumeTurnID, buffer.Sink)
			resumeTurnID = ""
		} else {
			currentRound := nextRound
			admissionToken := conversation.admissionTokenForGoal(ctx, goal.ID, agentgoal.AdmissionReserved, agentgoal.AdmissionTurnStarted)
			last, err = conversation.runInnerTurn(ctx, input, agentruntime.ContextHints{
				GoalID: goal.ID, GoalText: goalContextText(goal), GoalRound: currentRound, AdmissionToken: admissionToken,
			}, buffer.Sink, func(turnID string) error {
				var startErr error
				round, startErr = conversation.goalStore().StartRound(ctx, goal.ID, currentRound, turnID, input)
				if startErr != nil {
					return startErr
				}
				if admissionToken != "" && currentRound == 1 {
					repository, ok := conversation.goalStore().(agentgoal.AdmissionRepository)
					if !ok {
						return errors.New("durable Goal admission repository is unavailable")
					}
					admission, err := repository.Get(ctx, admissionToken)
					if err != nil {
						return err
					}
					if _, err := repository.BindRound(ctx, admissionToken, goal.ID, currentRound, admission.Version); err != nil {
						return fmt.Errorf("bind Goal admission round: %w", err)
					}
				}
				goal.Round = currentRound
				if reason, reached, err := guard.Reached(ctx, goal, currentRound); err != nil {
					return err
				} else if reached && reason == agentgoal.StopBudgetExhausted {
					return goalBudgetPauseError()
				}
				return nil
			})
		}
		if err != nil {
			if last.TurnID == "" {
				last.TurnID = round.TurnID
			}
			var suspended *orchestration.WaitSuspendedError
			if errors.As(err, &suspended) && ctx.Err() == nil {
				return last, err
			}
			return conversation.stopGoalError(ctx, goal, last, buffer, err)
		}
		candidate := ""
		if last.Final != nil {
			candidate = last.Final.Content
		}
		report, found, err := conversation.goalStore().ReportForTurn(ctx, conversation.sessionID, last.TurnID)
		if err != nil {
			return conversation.stopGoalError(ctx, goal, last, buffer, err)
		}
		var reportPointer *agentgoal.Report
		if found {
			reportPointer = &report
		}
		if err := goalPhase(ctx, sink, goal, round.Number, "verifying", ""); err != nil {
			return conversation.stopGoalError(ctx, goal, last, buffer, err)
		}
		config, _, _, _ := conversation.factory.GoalRuntime()
		assessmentCtx, assessmentCancel := context.WithTimeout(ctx, config.WithDefaults().EvaluationTimeout)
		verdict, err := evaluator.Evaluate(assessmentCtx, agentgoal.Evaluation{Goal: goal, Round: round, Candidate: candidate, Report: reportPointer})
		if assessmentCtx.Err() != nil {
			err = fmt.Errorf("goal assessment did not finish within its deadline: %w", assessmentCtx.Err())
		}
		assessmentCancel()
		if err != nil {
			return conversation.stopGoalError(ctx, goal, last, buffer, err)
		}
		if err := verdict.Validate(); err != nil {
			return conversation.stopGoalError(ctx, goal, last, buffer, err)
		}
		if _, err := conversation.goalStore().FinishRound(ctx, goal.ID, round.Number, verdict, candidate); err != nil {
			return conversation.stopGoalError(ctx, goal, last, buffer, err)
		}
		updated, err := conversation.goalStore().GetGoal(ctx, goal.ID)
		if err != nil {
			return conversation.stopGoalError(ctx, goal, last, buffer, err)
		}
		goal = updated

		transition, err := conversation.applyGoalVerdict(ctx, goal, verdict, last, buffer, guard)
		if err != nil || !transition.continueRound {
			return transition.result, err
		}
		input = transition.nextInput
		if err := goalPhase(ctx, sink, goal, goal.Round+1, "retrying", verdict.Feedback); err != nil {
			return conversation.stopGoalError(ctx, goal, last, buffer, err)
		}
	}
}

// admissionTokenForGoal finds the single host-issued admission that owns a
// Goal while its first Turn/round is being bound. It intentionally searches
// only non-terminal states; an already consumed token must never be reused.
func (conversation *Conversation) admissionTokenForGoal(ctx context.Context, goalID string, states ...agentgoal.AdmissionState) string {
	repository, ok := conversation.goalStore().(agentgoal.AdmissionRepository)
	if !ok {
		return ""
	}
	for _, state := range states {
		admissions, err := repository.List(ctx, state)
		if err != nil {
			return ""
		}
		for _, admission := range admissions {
			if admission.GoalID == goalID {
				return admission.Token
			}
		}
	}
	return ""
}

func (conversation *Conversation) goalStore() agentgoal.Store {
	_, store, _, _ := conversation.factory.GoalRuntime()
	return store
}

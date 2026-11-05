package conversation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
)

// ResumeTurn restores the existing turn's execution or Goal assessment phase.
// Incomplete execution uses its durable Eino checkpoint without a second user
// message; completed execution supplies its candidate without rerunning tools.
func (conversation *Conversation) ResumeTurn(ctx context.Context, turnID string, sink agentruntime.EventSink) (agentruntime.Result, error) {
	if strings.TrimSpace(turnID) == "" {
		return agentruntime.Result{}, errors.New("conversation resume requires a turn ID")
	}
	conversation.mutex.Lock()
	defer conversation.mutex.Unlock()
	return conversation.resumeTurn(ctx, turnID, sink)
}

func (conversation *Conversation) resumeTurn(ctx context.Context, turnID string, sink agentruntime.EventSink) (agentruntime.Result, error) {
	_, goalStore, evaluator, guard := conversation.factory.GoalRuntime()
	requiresGoalBinding, err := conversation.turnRequiresGoalBinding(ctx, turnID)
	if err != nil {
		// A terminal Goal is already a durable outcome. Returning it is safe
		// even when a legacy caller created the Goal round before the turn
		// start event; no model or tool execution is possible from this path.
		if goalStore != nil {
			if goal, _, found, goalErr := goalStore.GoalForTurn(ctx, conversation.sessionID, turnID); goalErr == nil && found {
				if goal.State != agentgoal.StateRunning {
					return agentruntime.Result{}, fmt.Errorf("goal %s already stopped (%s); cannot resume its execution", goal.ID, goal.State)
				}
				if !goal.StartedAt.IsZero() && time.Now().UTC().After(goal.StartedAt.Add(goal.MaxWallTime)) {
					return conversation.exhaustGoal(ctx, goal, agentruntime.Result{TurnID: turnID}, agentgoal.StopMaxWallTime, &candidateEventBuffer{downstream: sink})
				}
			}
		}
		return agentruntime.Result{}, err
	}
	if err := conversation.factory.CheckTurnRecovery(ctx, conversation.sessionID, turnID); err != nil {
		return agentruntime.Result{}, err
	}
	if goalStore == nil && requiresGoalBinding {
		return agentruntime.Result{}, errors.New("Goal-mode turn has no Goal store; refusing recovery without its round binding")
	}
	if goalStore != nil {
		goal, round, found, err := goalStore.GoalForTurn(ctx, conversation.sessionID, turnID)
		if err != nil {
			return agentruntime.Result{}, err
		}
		if found && goal.State == agentgoal.StateRunning {
			if err := conversation.validateTurnAdmission(ctx, turnID, goal.ID, round.Number); err != nil {
				return agentruntime.Result{}, err
			}
			evaluator, err = conversation.factory.GoalEvaluator(goal.Assessment)
			if err != nil {
				return agentruntime.Result{}, err
			}
			return conversation.resumeGoal(ctx, goal, round, evaluator, guard, sink)
		}
		if found {
			return agentruntime.Result{}, fmt.Errorf("goal %s already stopped (%s); cannot resume its execution", goal.ID, goal.State)
		}
		if requiresGoalBinding {
			return agentruntime.Result{}, errors.New("Goal-mode turn has no durable Goal round binding; refusing ordinary execution recovery")
		}
	}
	auto, err := conversation.factory.AutoAdmissionForTurn(ctx, conversation.sessionID, turnID)
	if err != nil {
		return agentruntime.Result{}, err
	}
	if auto {
		return conversation.runAutoTurn(ctx, "", turnID, sink)
	}
	result, err := conversation.resumeInnerTurn(ctx, turnID, sink)
	if err == nil {
		conversation.completedTurns++
	}
	return result, err
}

func (conversation *Conversation) validateTurnAdmission(ctx context.Context, turnID, goalID string, round int) error {
	records, err := conversation.factory.TurnRecords(ctx, conversation.sessionID)
	if err != nil {
		return err
	}
	var token string
	for _, record := range records {
		if record.TurnID == turnID {
			token = record.AdmissionToken
			break
		}
	}
	if token == "" {
		return nil
	}
	repository, ok := conversation.goalStore().(agentgoal.AdmissionRepository)
	if !ok {
		return errors.New("Goal admission repository is unavailable")
	}
	admission, err := repository.Get(ctx, token)
	if err != nil {
		return err
	}
	if admission.State == agentgoal.AdmissionTurnStarted {
		// StartRound and BindRound are separate durable ports. If the process
		// stopped between them, the round itself is proof enough to complete
		// the token transition after checking the exact Goal/Turn identity.
		if goal, round, found, lookupErr := conversation.goalStore().GoalForTurn(ctx, conversation.sessionID, turnID); lookupErr != nil {
			return lookupErr
		} else if found && goal.ID == admission.GoalID && round.TurnID == turnID {
			_, bindErr := repository.BindRound(ctx, token, goal.ID, round.Number, admission.Version)
			return bindErr
		}
	}
	if admission.State == agentgoal.AdmissionTurnStarted {
		if admission.GoalID != goalID || round <= 0 {
			return fmt.Errorf("Goal admission %s does not match recovered round", token)
		}
		var bindErr error
		admission, bindErr = repository.BindRound(ctx, token, goalID, round, admission.Version)
		if bindErr != nil {
			return fmt.Errorf("repair Goal admission round: %w", bindErr)
		}
	}
	if admission.State != agentgoal.AdmissionRoundStarted && admission.State != agentgoal.AdmissionConsumed {
		return fmt.Errorf("Goal admission %s is %s; refusing recovery", token, admission.State)
	}
	return nil
}

func (conversation *Conversation) turnRequiresGoalBinding(ctx context.Context, turnID string) (bool, error) {
	records, err := conversation.factory.TurnRecords(ctx, conversation.sessionID)
	if err != nil {
		return false, err
	}
	for _, record := range records {
		if record.TurnID == turnID {
			return record.RequiresGoalBinding(), nil
		}
	}
	return false, fmt.Errorf("turn %s has no durable start", turnID)
}

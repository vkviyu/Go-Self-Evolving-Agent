package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

var errGoalDeadline = errors.New("task wall-time limit reached")

type completedGoalRound struct {
	result agentruntime.Result
	buffer *candidateEventBuffer
}

// runAutoTurn starts a normal Eino turn and adopts that SAME turn if goal_start
// registers a task. No classifier request, discarded tool work, or second agent
// engine is required. The callback never changes shared Factory state.
func (conversation *Conversation) runAutoTurn(ctx context.Context, input, resumeTurnID string, sink agentruntime.EventSink) (agentruntime.Result, error) {
	config, store, evaluator, guard := conversation.factory.GoalRuntime()
	if store == nil {
		return agentruntime.Result{}, errors.New("task admission requires a durable goal store")
	}
	// A resumed auto checkpoint may predate a disabled current default.
	if evaluator == nil {
		var err error
		evaluator, err = conversation.factory.GoalEvaluator(config.Assessment)
		if err != nil {
			return agentruntime.Result{}, err
		}
	}
	started := time.Now().UTC()
	turnID := resumeTurnID
	requested := false
	if resumeTurnID != "" {
		var err error
		input, started, err = conversation.factory.TurnInput(ctx, conversation.sessionID, resumeTurnID)
		if err != nil {
			return agentruntime.Result{}, err
		}
		requested, err = conversation.factory.AdmissionRequestedForTurn(ctx, conversation.sessionID, resumeTurnID)
		if err != nil {
			return agentruntime.Result{}, err
		}
	}
	admissionGuard := agentgoal.NewAdmissionGuard(store, conversation.sessionID, turnID, requested)
	ctx = tool.WithTurnGuard(ctx, admissionGuard)
	ctx, cancel := context.WithDeadlineCause(ctx, started.Add(config.WithDefaults().MaxWallTime), errGoalDeadline)
	defer cancel()
	var mu sync.Mutex
	var admitted agentgoal.Goal
	var round agentgoal.Round
	var admissionToken string
	ctx = agentgoal.WithAdmission(ctx, func(callCtx context.Context, invocation tool.Invocation, contract agentgoal.Contract) (agentgoal.Goal, error) {
		mu.Lock()
		defer mu.Unlock()
		if invocation.SessionID != conversation.sessionID || invocation.TurnID != turnID || turnID == "" {
			return agentgoal.Goal{}, errors.New("task admission identity mismatch")
		}
		actor := conversation.factory.ActorScope()
		request := agentgoal.CreateRequest{
			ID: "goal-" + turnID, SessionID: conversation.sessionID, TeamID: actor.TeamID, AgentID: actor.AgentID,
			Text: input, Contract: &contract, StartedAt: started, Assessment: config.Assessment,
			MaxRounds: config.MaxRounds, MaxWallTime: config.MaxWallTime,
		}
		probe := agentgoal.Goal{TeamID: actor.TeamID, MaxRounds: config.MaxRounds, MaxWallTime: config.MaxWallTime, StartedAt: started}
		if reason, reached, err := guard.Reached(callCtx, probe, 1); err != nil {
			return agentgoal.Goal{}, err
		} else if reached {
			return agentgoal.Goal{}, fmt.Errorf("cannot admit task: %s", reason)
		}
		if repository, ok := store.(agentgoal.AdmissionRepository); ok {
			tokenID, err := agentgoal.NewAdmissionToken()
			if err != nil {
				return agentgoal.Goal{}, err
			}
			admissionToken = tokenID
			contractBytes, marshalErr := json.Marshal(contract)
			if marshalErr != nil {
				return agentgoal.Goal{}, marshalErr
			}
			prepared, err := repository.Prepare(callCtx, agentgoal.AdmissionRequest{
				Token: admissionToken, GoalID: request.ID, SessionID: string(request.SessionID), TeamID: request.TeamID, AgentID: request.AgentID,
				InputDigest: admissionDigest(input), ContractDigest: admissionDigest(string(contractBytes)), ConfigDigest: admissionDigest(fmt.Sprintf("%s/%s/%d/%s", config.Assessment, config.Activation, config.MaxRounds, config.MaxWallTime)),
			})
			if err != nil {
				return agentgoal.Goal{}, fmt.Errorf("prepare Goal admission: %w", err)
			}
			admissionToken = prepared.Token
		}
		goal, firstRound, err := store.AdmitGoal(callCtx, request, turnID)
		if err == nil {
			admitted, round = goal, firstRound
			if admissionToken != "" {
				repository, ok := store.(agentgoal.AdmissionRepository)
				if !ok {
					return agentgoal.Goal{}, errors.New("durable Goal admission repository is unavailable")
				}
				admission, bindErr := repository.Get(callCtx, admissionToken)
				if bindErr != nil {
					return agentgoal.Goal{}, bindErr
				}
				if _, bindErr = repository.BindTurn(callCtx, admissionToken, turnID, admissionDigest(input), admission.Version); bindErr != nil {
					return agentgoal.Goal{}, bindErr
				}
				admission, bindErr = repository.Get(callCtx, admissionToken)
				if bindErr != nil {
					return agentgoal.Goal{}, bindErr
				}
				if _, bindErr = repository.BindRound(callCtx, admissionToken, goal.ID, firstRound.Number, admission.Version); bindErr != nil {
					return agentgoal.Goal{}, bindErr
				}
			}
		} else if admissionToken != "" {
			if repository, ok := store.(agentgoal.AdmissionRepository); ok {
				if admission, getErr := repository.Get(callCtx, admissionToken); getErr == nil {
					_, _ = repository.MarkUnavailable(callCtx, admissionToken, admission.Version, err.Error())
				}
			}
		}
		return goal, err
	})
	buffer := &candidateEventBuffer{downstream: sink}
	announced := false
	autoSink := func(eventCtx context.Context, event agentruntime.Event) error {
		mu.Lock()
		goal := admitted
		mu.Unlock()
		if goal.ID == "" && !admissionGuard.Requested() {
			if sink != nil {
				return sink(eventCtx, event)
			}
			return nil
		}
		if goal.ID != "" && !announced {
			announced = true
			if err := goalPhase(eventCtx, sink, goal, 1, "executing", ""); err != nil {
				return err
			}
		}
		return buffer.Sink(eventCtx, event)
	}
	var result agentruntime.Result
	var err error
	if resumeTurnID != "" {
		result, err = conversation.resumeInnerTurn(ctx, resumeTurnID, autoSink)
	} else {
		result, err = conversation.runInnerTurn(ctx, input, agentruntime.ContextHints{}, autoSink, func(id string) error {
			turnID = id
			admissionGuard.BindTurn(id)
			return nil
		})
	}
	mu.Lock()
	goal, currentRound := admitted, round
	mu.Unlock()
	if result.TurnID == "" {
		result.TurnID = turnID
	}
	if goal.ID == "" {
		if err == nil {
			conversation.completedTurns++
		}
		return result, err
	}
	if err != nil {
		var suspended *orchestration.WaitSuspendedError
		if errors.As(err, &suspended) && ctx.Err() == nil {
			return result, err
		}
		return conversation.stopGoalError(ctx, goal, result, buffer, err)
	}
	return conversation.runGoal(ctx, goal, currentRound, "", "", evaluator, guard, sink, &completedGoalRound{result: result, buffer: buffer})
}

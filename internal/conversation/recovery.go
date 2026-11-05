package conversation

import (
	"context"
	"errors"
	"fmt"

	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

// Recovery is discovery metadata, not authorization or a compatibility check.
// The selected turn is revalidated at execution time.
type Recovery struct {
	TurnID string `json:"turn_id,omitempty"`
	GoalID string `json:"goal_id,omitempty"`
	Phase  string `json:"phase"`
	Reason string `json:"reason,omitempty"`
}

func (conversation *Conversation) Recoveries(ctx context.Context) ([]Recovery, error) {
	conversation.mutex.Lock()
	defer conversation.mutex.Unlock()
	turns, err := conversation.factory.TurnRecords(ctx, conversation.sessionID)
	if err != nil {
		return nil, err
	}
	_, goals, _, _ := conversation.factory.GoalRuntime()
	return recoveriesForTurns(ctx, conversation.sessionID, turns, goals)
}

// DiscoverRecoveries is the offline read model, shared by chat and CLI.
func DiscoverRecoveries(ctx context.Context, id session.ID, events session.EventStore, checkpoints session.CheckpointStore, goals agentgoal.Store) ([]Recovery, error) {
	turns, err := agentruntime.LoadTurnRecords(ctx, events, checkpoints, id)
	if err != nil {
		return nil, err
	}
	return recoveriesForTurns(ctx, id, turns, goals)
}

func recoveriesForTurns(ctx context.Context, id session.ID, turns []agentruntime.TurnRecord, goals agentgoal.Store) ([]Recovery, error) {
	var candidates []Recovery
	seenGoals := make(map[string]bool)
	seenAdmissions := make(map[string]bool)
	for _, turn := range turns {
		if turn.Abandoned || turn.Archived {
			continue
		}
		candidate := Recovery{TurnID: turn.TurnID, Phase: "execution"}
		missingGoalBinding := turn.RequiresGoalBinding()
		if goals != nil {
			goal, round, found, err := goals.GoalForTurn(ctx, id, turn.TurnID)
			if err != nil {
				return nil, err
			}
			if found {
				missingGoalBinding = false
				if goal.State != agentgoal.StateRunning || round.Number != goal.Round {
					continue
				}
				seenGoals[goal.ID] = true
				candidate.GoalID = goal.ID
				if round.CompletedAt != nil {
					candidate.Phase = "verdict"
				} else if turn.Completed {
					candidate.Phase = "assessment"
				}
			}
		}
		if turn.AdmissionToken != "" && goals != nil {
			seenAdmissions[turn.AdmissionToken] = true
			if repository, ok := goals.(agentgoal.AdmissionRepository); ok {
				admission, admissionErr := repository.Get(ctx, turn.AdmissionToken)
				if admissionErr != nil {
					return nil, admissionErr
				}
				boundRound := false
				if admission.State == agentgoal.AdmissionTurnStarted {
					if goal, round, found, lookupErr := goals.GoalForTurn(ctx, id, turn.TurnID); lookupErr != nil {
						return nil, lookupErr
					} else if found && goal.ID == admission.GoalID && round.TurnID == turn.TurnID {
						boundRound = true
					}
				}
				if !boundRound && admission.State != agentgoal.AdmissionRoundStarted && admission.State != agentgoal.AdmissionConsumed {
					candidate.Phase, candidate.Reason = "unavailable", "Goal admission is not bound to a durable round"
					candidates = append(candidates, candidate)
					continue
				}
			}
		}
		if missingGoalBinding {
			candidate.Phase, candidate.Reason = "unavailable", "Goal-mode turn has no durable Goal round binding; ordinary execution recovery is refused"
			candidates = append(candidates, candidate)
			continue
		}
		if turn.Completed && candidate.GoalID == "" {
			continue
		}
		if candidate.Phase == "execution" && !turn.HasCheckpoint && !turn.HasJournal {
			candidate.Phase, candidate.Reason = "unavailable", "checkpoint missing; inspect prior effects before starting new work"
		}
		candidates = append(candidates, candidate)
	}
	if repository, ok := goals.(agentgoal.AdmissionRepository); ok {
		for _, state := range []agentgoal.AdmissionState{agentgoal.AdmissionReserved, agentgoal.AdmissionTurnStarted} {
			admissions, err := repository.List(ctx, state)
			if err != nil {
				return nil, err
			}
			for _, admission := range admissions {
				if seenAdmissions[admission.Token] {
					continue
				}
				candidates = append(candidates, Recovery{TurnID: admission.TurnID, GoalID: admission.GoalID, Phase: "unavailable", Reason: "Goal admission has no matching durable Turn binding"})
			}
		}
	}
	if reader, ok := goals.(agentgoal.PendingRoundReader); ok {
		rounds, err := reader.PendingRounds(ctx, id)
		if err != nil {
			return nil, err
		}
		for _, round := range rounds {
			if seenGoals[round.GoalID] {
				continue
			}
			candidates = append(candidates, Recovery{GoalID: round.GoalID, TurnID: round.TurnID, Phase: "unavailable", Reason: "Goal has no durable turn start; automatic re-execution is refused"})
		}
	}
	return candidates, nil
}

// RecoverRun is the Supervisor adapter boundary. It never substitutes a fresh
// RunTurn when a prior run lacks a compatible checkpoint or durable result.
func (conversation *Conversation) RecoverRun(ctx context.Context, runID orchestration.RunID, sink agentruntime.EventSink) (agentruntime.Result, error) {
	conversation.mutex.Lock()
	defer conversation.mutex.Unlock()
	if runID == "" || conversation.factory.ActorScope().RunID != runID {
		return agentruntime.Result{}, errors.New("recovery requires the original bound Run ID")
	}
	turns, err := conversation.factory.TurnRecords(ctx, conversation.sessionID)
	if err != nil {
		return agentruntime.Result{}, err
	}
	var latest *agentruntime.TurnRecord
	for index := range turns {
		if turns[index].RunID == runID {
			latest = &turns[index]
		}
	}
	if latest == nil {
		return agentruntime.Result{}, fmt.Errorf("run %s has no durable turn binding; refusing to start replacement work", runID)
	}
	if err := conversation.factory.CheckTurnRecovery(ctx, conversation.sessionID, latest.TurnID); err != nil {
		return agentruntime.Result{}, err
	}
	_, goals, _, _ := conversation.factory.GoalRuntime()
	if goals == nil && latest.RequiresGoalBinding() {
		return agentruntime.Result{}, errors.New("Goal-mode run has no Goal store; refusing recovery without its round binding")
	}
	if goals != nil {
		goal, round, found, err := goals.GoalForTurn(ctx, conversation.sessionID, latest.TurnID)
		if err != nil {
			return agentruntime.Result{}, err
		}
		if found {
			if round.Number != goal.Round {
				return agentruntime.Result{}, errors.New("latest bound turn is not the current Goal round")
			}
			if goal.State != agentgoal.StateRunning {
				return agentruntime.Result{TurnID: latest.TurnID, Final: schema.AssistantMessage(goal.Result, nil), GoalID: goal.ID, GoalState: goal.State, GoalDecision: goal.Decision, GoalStopReason: goal.StopReason, GoalReason: goal.Reason, GoalRounds: goal.Round}, nil
			}
			return conversation.resumeTurn(ctx, latest.TurnID, sink)
		}
		if latest.RequiresGoalBinding() {
			return agentruntime.Result{}, errors.New("Goal-mode run has no durable Goal round binding; refusing ordinary execution recovery")
		}
	}
	if latest.Completed {
		result, found, err := conversation.factory.CompletedTurn(ctx, conversation.sessionID, latest.TurnID)
		if err == nil && !found {
			err = errors.New("completed turn outcome disappeared")
		}
		return result, err
	}
	return conversation.resumeTurn(ctx, latest.TurnID, sink)
}

package conversation

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
)

// RunTurn prepares context, executes one turn and refreshes durable history.
// The mutex deliberately spans the model call: a conversation cannot execute
// overlapping turns against the same ordered event stream.
func (conversation *Conversation) RunTurn(ctx context.Context, input string, sink agentruntime.EventSink) (agentruntime.Result, error) {
	if strings.TrimSpace(input) == "" {
		return agentruntime.Result{}, errors.New("conversation input must not be empty")
	}
	conversation.mutex.Lock()
	defer conversation.mutex.Unlock()
	goalMode, goalStore, evaluator, guard := conversation.factory.GoalRuntime()
	if goalMode.RequiresInitialGoal() {
		goalID, err := agentgoal.NewID()
		if err != nil {
			return agentruntime.Result{}, err
		}
		actor := conversation.factory.ActorScope()
		request := agentgoal.CreateRequest{
			ID: goalID, SessionID: conversation.sessionID, TeamID: actor.TeamID, AgentID: actor.AgentID,
			Text: input, Assessment: goalMode.Assessment, MaxRounds: goalMode.MaxRounds, MaxWallTime: goalMode.MaxWallTime,
		}
		admissionToken := ""
		if repository, ok := goalStore.(agentgoal.AdmissionRepository); ok {
			tokenID, tokenErr := agentgoal.NewAdmissionToken()
			if tokenErr != nil {
				return agentruntime.Result{}, tokenErr
			}
			admissionToken = tokenID
			admission, prepareErr := repository.Prepare(ctx, agentgoal.AdmissionRequest{
				Token: admissionToken, GoalID: goalID, SessionID: string(conversation.sessionID), TeamID: actor.TeamID, AgentID: actor.AgentID,
				InputDigest: admissionDigest(input), ContractDigest: admissionDigest(""), ConfigDigest: admissionDigest(fmt.Sprintf("%s/%s/%d/%s", goalMode.Assessment, goalMode.Activation, goalMode.MaxRounds, goalMode.MaxWallTime)),
			})
			if prepareErr != nil {
				return agentruntime.Result{}, fmt.Errorf("prepare Goal admission: %w", prepareErr)
			}
			admissionToken = admission.Token
		}
		goal, err := goalStore.CreateGoal(ctx, request)
		if err != nil {
			if admissionToken != "" {
				if repository, ok := goalStore.(agentgoal.AdmissionRepository); ok {
					if admission, getErr := repository.Get(ctx, admissionToken); getErr == nil {
						_, _ = repository.MarkUnavailable(ctx, admissionToken, admission.Version, err.Error())
					}
				}
			}
			return agentruntime.Result{}, err
		}
		return conversation.runGoal(ctx, goal, agentgoal.Round{}, input, "", evaluator, guard, sink, nil)
	}
	if goalMode.Enabled {
		return conversation.runAutoTurn(ctx, input, "", sink)
	}
	result, err := conversation.runInnerTurn(ctx, input, agentruntime.ContextHints{}, sink, nil)
	if err == nil {
		conversation.completedTurns++
	}
	return result, err
}

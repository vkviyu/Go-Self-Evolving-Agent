package conversation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
)

// Inner execution is always called with the Conversation mutex held. Context
// preparation runs at the recorded model boundary, after input is durable.
func (conversation *Conversation) runInnerTurn(ctx context.Context, input string, hints agentruntime.ContextHints, sink agentruntime.EventSink, beforeRun func(string) error) (agentruntime.Result, error) {
	factory := conversation.factory
	if hints.GoalID != "" {
		factory = factory.WithActiveGoal()
	} else if !agentgoal.AdmissionEnabled(ctx) {
		factory = factory.WithoutGoalMode()
	}
	userMessage := schema.UserMessage(input)
	messages := make([]*schema.Message, 0, len(conversation.history)+1)
	messages = append(messages, cloneMessages(conversation.history)...)
	messages = append(messages, userMessage)
	if err := conversation.refreshHistory(ctx, "refresh history after context preparation"); err != nil {
		return agentruntime.Result{}, err
	}
	turn, err := factory.NewTurnWithAdmission(ctx, conversation.sessionID, hints.AdmissionToken)
	if err != nil {
		return agentruntime.Result{}, err
	}
	if hints.AdmissionToken != "" {
		repository, ok := factory.GoalRuntimeStore().(agentgoal.AdmissionRepository)
		if !ok {
			return agentruntime.Result{}, errors.New("durable Goal admission repository is unavailable")
		}
		turn.SetAdmission(hints.AdmissionToken, func(bindCtx context.Context, turnID, input string) error {
			admission, err := repository.Get(bindCtx, hints.AdmissionToken)
			if err != nil {
				return err
			}
			_, err = repository.BindTurn(bindCtx, hints.AdmissionToken, turnID, admissionDigest(input), admission.Version)
			return err
		})
	}
	result, err := turn.RunWithAdmission(ctx, messages, sink, beforeRun)
	if err != nil {
		return agentruntime.Result{}, err
	}
	if err := conversation.refreshHistory(ctx, "refresh session history"); err != nil {
		return agentruntime.Result{}, err
	}
	return result, nil
}

func admissionDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func (conversation *Conversation) resumeInnerTurn(ctx context.Context, turnID string, sink agentruntime.EventSink) (agentruntime.Result, error) {
	turn, err := conversation.factory.ResumeTurn(ctx, conversation.sessionID, turnID)
	if err != nil {
		return agentruntime.Result{}, err
	}
	result, err := turn.Resume(ctx, sink)
	if err != nil {
		return agentruntime.Result{}, err
	}
	if err := conversation.refreshHistory(ctx, "refresh session history after resume"); err != nil {
		return agentruntime.Result{}, err
	}
	return result, nil
}

package conversation

import (
	"context"

	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
)

// Inner execution is always called with the Conversation mutex held. Context
// preparation runs at the recorded model boundary, after input is durable.
func (conversation *Conversation) runInnerTurn(ctx context.Context, input string, hints agentruntime.ContextHints, sink agentruntime.EventSink, beforeRun func(string) error) (agentruntime.Result, error) {
	ctx = agentruntime.WithLiveSink(ctx, sink)
	factory := conversation.factory
	userMessage := schema.UserMessage(input)
	messages := make([]*schema.Message, 0, len(conversation.history)+1)
	messages = append(messages, cloneMessages(conversation.history)...)
	messages = append(messages, userMessage)
	freshSession := conversation.completedTurns == 0 && len(conversation.history) == 0
	if !freshSession {
		if err := emit(sink, ctx, agentruntime.Event{Type: agentruntime.EventConversationHistoryLoading}); err != nil {
			return agentruntime.Result{}, err
		}
		if err := conversation.refreshHistory(ctx, "refresh history after context preparation"); err != nil {
			return agentruntime.Result{}, err
		}
	}
	turn, err := factory.NewTurn(ctx, conversation.sessionID)
	if err != nil {
		return agentruntime.Result{}, err
	}
	result, err := turn.RunWithBefore(ctx, messages, sink, beforeRun)
	if err != nil {
		return agentruntime.Result{}, err
	}
	if err := conversation.refreshHistory(ctx, "refresh session history"); err != nil {
		return agentruntime.Result{}, err
	}
	return result, nil
}

func (conversation *Conversation) resumeInnerTurn(ctx context.Context, turnID string, sink agentruntime.EventSink) (agentruntime.Result, error) {
	ctx = agentruntime.WithLiveSink(ctx, sink)
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

package conversation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

// Inner execution is always called with the Conversation mutex held. Context
// preparation runs at the recorded model boundary, after input is durable.
func (conversation *Conversation) runInnerTurn(ctx context.Context, input string, hints agentruntime.ContextHints, sink agentruntime.EventSink, beforeRun func(string) error) (agentruntime.Result, error) {
	ctx = agentruntime.WithLiveSink(ctx, sink)
	factory := conversation.factory
	freshSession := conversation.completedTurns == 0 && len(conversation.history) == 0
	if !freshSession {
		if err := emit(sink, ctx, agentruntime.Event{Type: agentruntime.EventConversationHistoryLoading}); err != nil {
			return agentruntime.Result{}, err
		}
		if err := conversation.refreshHistory(ctx, "refresh history after context preparation"); err != nil {
			return agentruntime.Result{}, err
		}
	}
	progress, err := factory.LoadInterruptedContext(ctx, conversation.sessionID)
	if err != nil {
		return agentruntime.Result{}, fmt.Errorf("load interrupted work: %w", err)
	}
	turn, err := factory.NewTurn(ctx, conversation.sessionID)
	if err != nil {
		return agentruntime.Result{}, err
	}
	messages := cloneMessages(conversation.history)
	if progress != nil {
		messages = append(messages, progress)
	}
	messages = append(messages, schema.UserMessage(input))
	result, err := turn.RunWithBefore(ctx, messages, sink, beforeRun)
	return conversation.finishTurnExecution(ctx, turn.ID(), sink, result, err)
}

// resumeInnerTurn opens the recorded turn, optionally selects it as foreground and resumes
// execution with a live event sink.
func (conversation *Conversation) resumeInnerTurn(ctx context.Context, turnID string, sink agentruntime.EventSink, selectForeground bool) (agentruntime.Result, error) {
	ctx = agentruntime.WithLiveSink(ctx, sink)
	turn, err := conversation.factory.ResumeTurn(ctx, conversation.sessionID, turnID)
	if errors.Is(err, tool.ErrSnapshotHostChanged) {
		return agentruntime.Result{}, fmt.Errorf("%w. %s", err, hostRecoveryGuidance)
	}
	if err != nil {
		return agentruntime.Result{}, err
	}
	if selectForeground {
		if err := conversation.factory.SelectForegroundTurn(ctx, conversation.sessionID, turnID); err != nil {
			return agentruntime.Result{}, fmt.Errorf("record foreground turn selection: %w", err)
		}
	}
	result, err := turn.Resume(ctx, sink)
	return conversation.finishTurnExecution(ctx, turnID, sink, result, err)
}

func (conversation *Conversation) finishTurnExecution(ctx context.Context, turnID string, sink agentruntime.EventSink, result agentruntime.Result, err error) (agentruntime.Result, error) {
	if retry := agentruntime.RetryableModelFailure(err); ctx.Err() == nil && retry != "" {
		retryEvent := agentruntime.EventModelToolFormatRetry
		if retry == orchestration.ExecutionTransport {
			retryEvent = agentruntime.EventModelTimeoutRetry
		}
		if err := emit(sink, ctx, agentruntime.Event{Type: retryEvent, TurnID: turnID}); err != nil {
			return agentruntime.Result{}, err
		}
		if retry == orchestration.ExecutionTransport {
			timer := time.NewTimer(250 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return agentruntime.Result{}, ctx.Err()
			case <-timer.C:
			}
		}
		turn, err := conversation.factory.ResumeTurn(ctx, conversation.sessionID, turnID)
		if err != nil {
			return agentruntime.Result{}, err
		}
		result, err = turn.RetryModelFailure(ctx, sink, retry)
		if err != nil {
			return agentruntime.Result{}, err
		}
	} else if err != nil {
		return agentruntime.Result{}, err
	}
	if err := conversation.refreshHistory(ctx, "refresh session history"); err != nil {
		return agentruntime.Result{}, err
	}
	return result, nil
}

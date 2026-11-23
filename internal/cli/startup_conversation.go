package cli

import (
	"context"
	"errors"
	"sync"

	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	conversationapi "github.com/vkviyu/go-self-evolving-agent/internal/conversation"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tui"
)

// startupConversation exposes the durable conversation while startup
// maintenance finishes in the background. Interactive input waits at the
// execution boundary, so the terminal can render without bypassing recovery.
type startupConversation struct {
	baseMu   sync.RWMutex
	base     tui.Conversation
	ready    <-chan struct{}
	readyErr func() error
}

func (conversation *startupConversation) await(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-conversation.ready:
		if err := ctx.Err(); err != nil {
			return err
		}
		if conversation.readyErr != nil {
			return conversation.readyErr()
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (conversation *startupConversation) setBase(base tui.Conversation) {
	conversation.baseMu.Lock()
	conversation.base = base
	conversation.baseMu.Unlock()
}

func (conversation *startupConversation) currentBase() tui.Conversation {
	conversation.baseMu.RLock()
	defer conversation.baseMu.RUnlock()
	return conversation.base
}

func (conversation *startupConversation) ID() session.ID {
	return conversation.currentBase().ID()
}

func (conversation *startupConversation) History() []*schema.Message {
	return conversation.currentBase().History()
}

func (conversation *startupConversation) CompletedTurns() int {
	return conversation.currentBase().CompletedTurns()
}

func (conversation *startupConversation) RunTurn(ctx context.Context, input string, sink agentruntime.EventSink) (agentruntime.Result, error) {
	if err := conversation.await(ctx); err != nil {
		return agentruntime.Result{}, err
	}
	return conversation.currentBase().RunTurn(ctx, input, sink)
}

func (conversation *startupConversation) ContextUsage() agentruntime.ContextUsage {
	if base, ok := conversation.currentBase().(tui.ContextConversation); ok {
		return base.ContextUsage()
	}
	return agentruntime.ContextUsage{}
}

func (conversation *startupConversation) ContextUsageFor(input string) agentruntime.ContextUsage {
	if base, ok := conversation.currentBase().(interface {
		ContextUsageFor(string) agentruntime.ContextUsage
	}); ok {
		return base.ContextUsageFor(input)
	}
	return conversation.ContextUsage()
}

func (conversation *startupConversation) Recoveries(ctx context.Context) ([]conversationapi.Recovery, error) {
	if err := conversation.await(ctx); err != nil {
		return nil, err
	}
	if base, ok := conversation.currentBase().(tui.RecoveryConversation); ok {
		return base.Recoveries(ctx)
	}
	return nil, nil
}

func (conversation *startupConversation) ResumeTurn(ctx context.Context, id string, sink agentruntime.EventSink) (agentruntime.Result, error) {
	if err := conversation.await(ctx); err != nil {
		return agentruntime.Result{}, err
	}
	if base, ok := conversation.currentBase().(tui.RecoveryConversation); ok {
		return base.ResumeTurn(ctx, id, sink)
	}
	return agentruntime.Result{}, errors.New("conversation does not support recovery")
}

func (conversation *startupConversation) Compress(ctx context.Context) (bool, agentruntime.ContextUsage, error) {
	return conversation.CompressWithSink(ctx, nil)
}

func (conversation *startupConversation) CompressWithSink(ctx context.Context, sink agentruntime.EventSink) (bool, agentruntime.ContextUsage, error) {
	if err := conversation.await(ctx); err != nil {
		return false, agentruntime.ContextUsage{}, err
	}
	if base, ok := conversation.currentBase().(tui.ContextConversation); ok {
		if eventBase, ok := base.(tui.ContextEventConversation); ok {
			return eventBase.CompressWithSink(ctx, sink)
		}
		return base.Compress(ctx)
	}
	return false, agentruntime.ContextUsage{}, errors.New("conversation does not support compression")
}

func (conversation *startupConversation) PendingBackground(ctx context.Context) (int, error) {
	if err := conversation.await(ctx); err != nil {
		return 0, err
	}
	if base, ok := conversation.currentBase().(tui.BackgroundConversation); ok {
		return base.PendingBackground(ctx)
	}
	return 0, nil
}

func (conversation *startupConversation) RunBackgroundTurn(ctx context.Context, sink agentruntime.EventSink) (agentruntime.Result, error) {
	if err := conversation.await(ctx); err != nil {
		return agentruntime.Result{}, err
	}
	if base, ok := conversation.currentBase().(tui.BackgroundConversation); ok {
		return base.RunBackgroundTurn(ctx, sink)
	}
	return agentruntime.Result{}, errors.New("conversation does not support background turns")
}

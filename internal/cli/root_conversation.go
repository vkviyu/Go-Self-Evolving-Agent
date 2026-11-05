package cli

import (
	"context"
	"fmt"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentmanager"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/conversation"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

// openRootConversation binds the root Agent's mailbox to its factory, settles
// messages of turns that committed before a restart and opens the session.
// Through that mailbox, child results reach the root as untrusted background
// data inside running turns or as the input of a wake turn, and they are
// acknowledged when the consuming turn commits.
func openRootConversation(ctx context.Context, factory *agentruntime.Factory, supervisor *agentmanager.Supervisor, root orchestration.RootThread) (*conversation.Conversation, error) {
	mailbox, err := supervisor.AgentMailbox(ctx, root.AgentID)
	if err != nil {
		return nil, fmt.Errorf("open root Agent mailbox: %w", err)
	}
	factory = factory.WithMailbox(mailbox)
	if err := factory.SettleBackground(ctx, root.SessionID); err != nil {
		return nil, err
	}
	return conversation.Open(ctx, factory, root.SessionID)
}

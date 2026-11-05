package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/approval"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

type runtimeEventMsg struct {
	event agentruntime.Event
}

type turnDoneMsg struct {
	result agentruntime.Result
	err    error
}

type compressDoneMsg struct {
	changed bool
	usage   agentruntime.ContextUsage
	err     error
}

type approvalRequestMsg struct {
	request approval.Request
}

type clipboardResultMsg struct {
	lines int
	err   error
}

type activityRefreshMsg struct {
	snapshot activitySnapshot
}

type activityTickMsg struct {
	sequence int
}

// agentActionMsg reports the outcome of an /agents panel action.
type agentActionMsg struct {
	notice string
	err    error
}

// agentTranscriptMsg carries a child Agent's transcript for the viewer.
type agentTranscriptMsg struct {
	agentID  orchestration.AgentID
	messages []*schema.Message
	err      error
}

type backgroundTickMsg struct{}

// backgroundPendingMsg reports how many Agent messages a wake would receive.
type backgroundPendingMsg struct {
	count int
	err   error
}

func runTurnCommand(
	ctx context.Context,
	lifetime context.Context,
	conversation Conversation,
	input string,
	events chan<- tea.Msg,
) tea.Cmd {
	return runOperationCommand(ctx, func(ctx context.Context, sink agentruntime.EventSink) (agentruntime.Result, error) {
		return conversation.RunTurn(ctx, input, sink)
	}, events, lifetime)
}

func runOperationCommand(ctx context.Context, run func(context.Context, agentruntime.EventSink) (agentruntime.Result, error), events chan<- tea.Msg, lifetime context.Context) tea.Cmd {
	return func() tea.Msg {
		result, err := run(ctx, func(eventContext context.Context, event agentruntime.Event) error {
			select {
			case events <- runtimeEventMsg{event: event}:
				return nil
			case <-eventContext.Done():
				return eventContext.Err()
			}
		})
		completion := turnDoneMsg{result: result, err: err}
		select {
		case events <- completion:
		case <-lifetime.Done():
		}
		return nil
	}
}

func waitForTurnEvent(ctx context.Context, events <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		select {
		case message := <-events:
			return message
		case <-ctx.Done():
			return nil
		}
	}
}

func waitForApproval(ctx context.Context, broker *approval.Broker) tea.Cmd {
	return func() tea.Msg {
		if broker == nil {
			return nil
		}
		select {
		case request, ok := <-broker.Requests():
			if !ok {
				return nil
			}
			return approvalRequestMsg{request: request}
		case <-broker.Done():
			return nil
		case <-ctx.Done():
			return nil
		}
	}
}

func runCompressCommand(ctx context.Context, conversation ContextConversation) tea.Cmd {
	return func() tea.Msg {
		changed, usage, err := conversation.Compress(ctx)
		return compressDoneMsg{changed: changed, usage: usage, err: err}
	}
}

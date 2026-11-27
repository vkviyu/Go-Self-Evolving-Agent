package tui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/approval"
	"github.com/vkviyu/go-self-evolving-agent/internal/experience"
)

type runtimeEventMsg struct {
	event agentruntime.Event
	at    time.Time
}

type memorySearchMsg struct {
	query       string
	items       []experience.MemoryResult
	err         error
	suggestions bool
	sequence    int
}

// StartupMsg is the public UI message for a startup channel notification.
type StartupMsg struct {
	State  StartupState
	Closed bool
}

func waitForStartup(ctx context.Context, startup <-chan StartupState) tea.Cmd {
	if startup == nil {
		return nil
	}
	return func() tea.Msg {
		select {
		case state, ok := <-startup:
			return StartupMsg{State: state, Closed: !ok}
		case <-ctx.Done():
			return nil
		}
	}
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

type backgroundTickMsg struct{}

// backgroundPendingMsg reports how many Agent messages a wake would receive.
type backgroundPendingMsg struct {
	count int
	err   error
}

func runOperationCommand(ctx context.Context, run func(context.Context, agentruntime.EventSink) (agentruntime.Result, error), events chan<- tea.Msg, lifetime context.Context) tea.Cmd {
	return func() tea.Msg {
		ctx = observeStorage(ctx, lifetime, events)
		result, err := run(ctx, func(eventContext context.Context, event agentruntime.Event) error {
			select {
			case events <- runtimeEventMsg{event: event, at: time.Now()}:
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

// There is one consumer per turn. If producers outran the terminal, paint
// already-queued updates together without waiting for more or starving input.
// A completion goes back to the main loop after all preceding deltas.
func (model *Model) updateTurnFrame(message tea.Msg) tea.Cmd {
	const maxFrameEvents = 32
	var commands []tea.Cmd
	var dirty bool
	next := waitForTurnEvent(model.ctx, model.turnEvents)
frame:
	for n := 1; ; n++ {
		switch m := message.(type) {
		case runtimeEventMsg:
			if cmd := model.refreshAfterAgentTool(m.event); cmd != nil {
				commands = append(commands, cmd)
			}
			previous := model.stage
			if model.applyRuntimeEvent(m.event) {
				dirty = true
			}
			if model.stage != previous {
				model.stageStartedAt = m.at
				if m.at.IsZero() {
					model.stageStartedAt = time.Now()
				}
			}
		case storageProgressMsg:
			model.applyStorageProgress(m)
		default:
			next = func() tea.Msg { return message }
			break frame
		}
		if n == maxFrameEvents {
			break
		}
		select {
		case message = <-model.turnEvents:
		default:
			break frame
		}
	}
	if dirty {
		model.refreshRootTranscript(false)
	}
	return tea.Batch(append(commands, next)...)
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

func runCompressCommand(ctx context.Context, conversation ContextConversation, events chan<- tea.Msg, lifetime context.Context) tea.Cmd {
	return func() tea.Msg {
		ctx = observeStorage(ctx, lifetime, events)
		sink := func(eventContext context.Context, event agentruntime.Event) error {
			select {
			case events <- runtimeEventMsg{event: event, at: time.Now()}:
				return nil
			case <-eventContext.Done():
				return eventContext.Err()
			}
		}
		var changed bool
		var usage agentruntime.ContextUsage
		var err error
		if eventConversation, ok := conversation.(ContextEventConversation); ok {
			changed, usage, err = eventConversation.CompressWithSink(ctx, sink)
		} else {
			changed, usage, err = conversation.Compress(ctx)
		}
		completion := compressDoneMsg{changed: changed, usage: usage, err: err}
		select {
		case events <- completion:
		case <-lifetime.Done():
		}
		return nil
	}
}

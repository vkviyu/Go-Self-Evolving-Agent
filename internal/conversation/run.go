package conversation

import (
	"context"
	"errors"
	"strings"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/observation"
)

// RunTurn prepares context, executes one turn and refreshes durable history.
// The mutex deliberately spans the model call: a conversation cannot execute
// overlapping turns against the same ordered event stream.
func (conversation *Conversation) RunTurn(ctx context.Context, input string, sink agentruntime.EventSink) (agentruntime.Result, error) {
	if strings.TrimSpace(input) == "" {
		return agentruntime.Result{}, errors.New("conversation input must not be empty")
	}
	ctx = observation.WithScope(ctx, observation.Scope{SessionID: string(conversation.sessionID)})
	if err := emit(sink, ctx, agentruntime.Event{Type: agentruntime.EventTurnStarting}); err != nil {
		return agentruntime.Result{}, err
	}
	if err := emit(sink, ctx, agentruntime.Event{Type: agentruntime.EventConversationLockWaiting}); err != nil {
		return agentruntime.Result{}, err
	}
	conversation.mutex.Lock()
	defer conversation.mutex.Unlock()
	if err := emit(sink, ctx, agentruntime.Event{Type: agentruntime.EventTurnBuilding}); err != nil {
		return agentruntime.Result{}, err
	}
	result, err := conversation.runInnerTurn(ctx, input, agentruntime.ContextHints{}, sink, nil)
	if err == nil {
		conversation.completedTurns++
	}
	return result, err
}

func emit(sink agentruntime.EventSink, ctx context.Context, event agentruntime.Event) error {
	observation.Emit(ctx, observation.Event{Component: "conversation", Name: string(event.Type), Scope: observation.Scope{TurnID: event.TurnID}})
	if sink == nil {
		return nil
	}
	return sink(ctx, event)
}

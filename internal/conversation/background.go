package conversation

import (
	"context"
	"errors"
	"fmt"

	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
)

// ErrBackgroundBudgetExhausted reports a wake skipped because the team has no
// budget left for another turn.
var ErrBackgroundBudgetExhausted = errors.New("team budget is exhausted")

// PendingBackground counts the Agent messages a wake turn would receive.
func (conversation *Conversation) PendingBackground(ctx context.Context) (int, error) {
	return conversation.factory.BackgroundAvailable(ctx)
}

// RunBackgroundTurn opens a turn with the available Agent messages as its
// input. It returns agentruntime.ErrNoBackgroundInput when none are available.
// A wake turn never admits a goal and does not count as a user turn.
func (conversation *Conversation) RunBackgroundTurn(ctx context.Context, sink agentruntime.EventSink) (agentruntime.Result, error) {
	conversation.mutex.Lock()
	defer conversation.mutex.Unlock()
	factory := conversation.factory.WithoutGoalMode()
	_, _, _, guard := factory.GoalRuntime()
	if guard.Budget != nil {
		budget, err := guard.Budget.Budget(ctx, factory.ActorScope().TeamID)
		if err != nil {
			return agentruntime.Result{}, fmt.Errorf("read team budget before wake: %w", err)
		}
		if budget.Exhausted {
			return agentruntime.Result{}, ErrBackgroundBudgetExhausted
		}
	}
	turn, err := factory.NewTurn(ctx, conversation.sessionID)
	if err != nil {
		return agentruntime.Result{}, err
	}
	input, err := factory.ClaimBackgroundInput(ctx, turn.ID())
	if err != nil {
		return agentruntime.Result{}, err
	}
	messages := make([]*schema.Message, 0, len(conversation.history)+1)
	messages = append(messages, cloneMessages(conversation.history)...)
	messages = append(messages, schema.UserMessage(input.Content))
	if err := conversation.refreshHistory(ctx, "refresh history after context preparation"); err != nil {
		return agentruntime.Result{}, err
	}
	result, err := turn.RunBackground(ctx, messages[:len(messages)-1], input, sink)
	if err != nil {
		return agentruntime.Result{}, err
	}
	if err := conversation.refreshHistory(ctx, "refresh session history"); err != nil {
		return agentruntime.Result{}, err
	}
	return result, nil
}

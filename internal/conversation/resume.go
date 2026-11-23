package conversation

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
)

// ResumeTurn restores an existing execution from its durable checkpoint or
// replay journal. A completed turn is returned without rerunning tools.
func (conversation *Conversation) ResumeTurn(ctx context.Context, turnID string, sink agentruntime.EventSink) (agentruntime.Result, error) {
	if strings.TrimSpace(turnID) == "" {
		return agentruntime.Result{}, errors.New("conversation resume requires a turn ID")
	}
	conversation.mutex.Lock()
	defer conversation.mutex.Unlock()
	return conversation.resumeTurn(ctx, turnID, sink)
}

func (conversation *Conversation) resumeTurn(ctx context.Context, turnID string, sink agentruntime.EventSink) (agentruntime.Result, error) {
	records, err := conversation.factory.TurnRecords(ctx, conversation.sessionID)
	if err != nil {
		return agentruntime.Result{}, err
	}
	var record *agentruntime.TurnRecord
	for index := range records {
		if records[index].TurnID == turnID {
			record = &records[index]
			break
		}
	}
	if record == nil {
		return agentruntime.Result{}, fmt.Errorf("turn %s has no durable start", turnID)
	}
	if record.RequiresExecutionRecovery() {
		return agentruntime.Result{}, errors.New("turn belongs to a removed execution admission protocol; recovery is fail-closed")
	}
	if err := conversation.factory.CheckTurnRecovery(ctx, conversation.sessionID, turnID); err != nil {
		return agentruntime.Result{}, err
	}
	if record.Completed {
		result, found, err := conversation.factory.CompletedTurn(ctx, conversation.sessionID, turnID)
		if err == nil && !found {
			err = errors.New("completed turn outcome disappeared")
		}
		return result, err
	}
	result, err := conversation.resumeInnerTurn(ctx, turnID, sink)
	if err == nil {
		conversation.completedTurns++
	}
	return result, err
}

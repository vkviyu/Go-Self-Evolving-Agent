package conversation

import (
	"context"
	"errors"
	"fmt"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/observation"
)

// ResumeTurn restores an existing execution from its durable checkpoint or
// replay journal. An empty ID selects the foreground turn from durable state.
// A completed turn selected by ID is returned without rerunning tools.
func (conversation *Conversation) ResumeTurn(ctx context.Context, turnID string, sink agentruntime.EventSink) (agentruntime.Result, error) {
	conversation.mutex.Lock()
	defer conversation.mutex.Unlock()
	records, err := conversation.factory.TurnRecords(ctx, conversation.sessionID)
	if err != nil {
		return agentruntime.Result{}, err
	}
	root := conversation.factory.ActorScope().ParentAgentID == ""
	if turnID == "" && root {
		record, err := foregroundRecoveryTarget(records)
		if err != nil {
			return agentruntime.Result{}, err
		}
		return conversation.resumeRecordedTurn(ctx, record, sink, record.ForegroundSequence == 0)
	}
	for _, record := range records {
		if record.TurnID == turnID {
			return conversation.resumeRecordedTurn(ctx, record, sink, root && record.IsForegroundRequest())
		}
	}
	return agentruntime.Result{}, fmt.Errorf("turn %s has no durable start", turnID)
}

func (conversation *Conversation) resumeRecordedTurn(ctx context.Context, record agentruntime.TurnRecord, sink agentruntime.EventSink, selectForeground bool) (agentruntime.Result, error) {
	turnID := record.TurnID
	ctx = observation.WithScope(ctx, observation.Scope{SessionID: string(conversation.sessionID), TurnID: turnID})
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
	if err := emit(sink, ctx, agentruntime.Event{Type: agentruntime.EventTurnResuming, TurnID: turnID}); err != nil {
		return agentruntime.Result{}, err
	}
	result, err := conversation.resumeInnerTurn(ctx, turnID, sink, selectForeground)
	if err == nil {
		conversation.completedTurns++
	}
	return result, err
}

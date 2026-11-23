package conversation

import (
	"context"
	"errors"
	"fmt"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

// Recovery is discovery metadata, not authorization or a compatibility check.
// The selected turn is revalidated at execution time.
type Recovery struct {
	TurnID string `json:"turn_id,omitempty"`
	Phase  string `json:"phase"`
	Reason string `json:"reason,omitempty"`
}

func (conversation *Conversation) Recoveries(ctx context.Context) ([]Recovery, error) {
	conversation.mutex.Lock()
	defer conversation.mutex.Unlock()
	turns, err := conversation.factory.TurnRecords(ctx, conversation.sessionID)
	if err != nil {
		return nil, err
	}
	return recoveriesForTurns(turns), nil
}

// DiscoverRecoveries is the offline read model, shared by chat and CLI. The
// optional final arguments are accepted for source compatibility with callers
// that still pass a repository; the removed Goal store is never consulted.
func DiscoverRecoveries(ctx context.Context, id session.ID, events session.EventStore, checkpoints session.CheckpointStore, _ ...any) ([]Recovery, error) {
	turns, err := agentruntime.LoadTurnRecords(ctx, events, checkpoints, id)
	if err != nil {
		return nil, err
	}
	return recoveriesForTurns(turns), nil
}

func recoveriesForTurns(turns []agentruntime.TurnRecord) []Recovery {
	var candidates []Recovery
	for _, turn := range turns {
		if turn.Abandoned || turn.Archived || turn.Completed {
			continue
		}
		candidate := Recovery{TurnID: turn.TurnID, Phase: "execution"}
		if turn.RequiresExecutionRecovery() {
			candidate.Phase = "unavailable"
			candidate.Reason = "turn belongs to a removed execution admission protocol; inspect prior effects before starting new work"
		} else if turn.LoopStopped {
			candidate.Phase = "unavailable"
			candidate.Reason = session.ErrRecoveryToolCycle.Error()
		} else if !turn.HasCheckpoint && !turn.HasJournal {
			candidate.Phase = "unavailable"
			candidate.Reason = "no checkpoint or turn journal; inspect prior effects before starting new work"
		}
		candidates = append(candidates, candidate)
	}
	return candidates
}

// RecoverRun is the Supervisor adapter boundary. It never substitutes a fresh
// RunTurn when a prior run lacks a compatible checkpoint or durable result.
func (conversation *Conversation) RecoverRun(ctx context.Context, runID orchestration.RunID, sink agentruntime.EventSink) (agentruntime.Result, error) {
	conversation.mutex.Lock()
	defer conversation.mutex.Unlock()
	if runID == "" || conversation.factory.ActorScope().RunID != runID {
		return agentruntime.Result{}, errors.New("recovery requires the original bound Run ID")
	}
	turns, err := conversation.factory.TurnRecords(ctx, conversation.sessionID)
	if err != nil {
		return agentruntime.Result{}, err
	}
	var latest *agentruntime.TurnRecord
	for index := range turns {
		if turns[index].RunID == runID {
			latest = &turns[index]
		}
	}
	if latest == nil {
		return agentruntime.Result{}, fmt.Errorf("run %s has no durable turn binding; refusing to start replacement work", runID)
	}
	if latest.RequiresExecutionRecovery() {
		return agentruntime.Result{}, errors.New("run belongs to a removed execution admission protocol; recovery is fail-closed")
	}
	if err := conversation.factory.CheckTurnRecovery(ctx, conversation.sessionID, latest.TurnID); err != nil {
		return agentruntime.Result{}, err
	}
	if latest.Completed {
		result, found, err := conversation.factory.CompletedTurn(ctx, conversation.sessionID, latest.TurnID)
		if err == nil && !found {
			err = errors.New("completed turn outcome disappeared")
		}
		return result, err
	}
	return conversation.resumeTurn(ctx, latest.TurnID, sink)
}

package conversation

import (
	"context"
	"errors"
	"fmt"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

// Recovery describes an interrupted turn. Execution still validates the
// original bindings, profile and authorization.
type Recovery struct {
	TurnID string `json:"turn_id,omitempty"`
	Phase  string `json:"phase"`
	Reason string `json:"reason,omitempty"`
	Prompt string `json:"prompt,omitempty"`
}

const hostRecoveryGuidance = "Use the original GSEA executable to resume this turn, or review prior effects and send a new instruction in this session."

func (conversation *Conversation) Recoveries(ctx context.Context) ([]Recovery, error) {
	conversation.mutex.Lock()
	defer conversation.mutex.Unlock()
	turns, err := conversation.factory.TurnRecords(ctx, conversation.sessionID)
	if err != nil {
		return nil, err
	}
	return recoveriesForTurns(ctx, turns, conversation.factory.CheckSnapshotHost)
}

// DiscoverRecoveries builds the same read model for an offline CLI caller.
func DiscoverRecoveries(ctx context.Context, id session.ID, events session.EventStore, checkpoints session.CheckpointStore, checkHost func(context.Context, string) error) ([]Recovery, error) {
	turns, err := agentruntime.LoadTurnRecords(ctx, events, checkpoints, id)
	if err != nil {
		return nil, err
	}
	return recoveriesForTurns(ctx, turns, checkHost)
}

func recoveriesForTurns(ctx context.Context, turns []agentruntime.TurnRecord, checkHost func(context.Context, string) error) ([]Recovery, error) {
	var candidates []Recovery
	checked := make(map[string]error)
	for _, turn := range turns {
		if turn.Abandoned || turn.Archived || turn.Completed {
			continue
		}
		candidate := Recovery{TurnID: turn.TurnID, Phase: "execution", Prompt: turn.OpeningPreview}
		if turn.RequiresExecutionRecovery() {
			candidate.Phase = "unavailable"
			candidate.Reason = "turn belongs to a removed execution admission protocol; inspect prior effects before starting new work"
		} else if turn.LoopStopped {
			candidate.Phase = "unavailable"
			candidate.Reason = session.ErrRecoveryToolCycle.Error()
		} else if !turn.HasCheckpoint && !turn.HasJournal {
			candidate.Phase = "unavailable"
			candidate.Reason = "no checkpoint or turn journal; inspect prior effects before starting new work"
		} else if turn.RuntimeSnapshotID == "" {
			candidate.Phase = "unavailable"
			candidate.Reason = "no recorded tool snapshot"
		} else {
			id := turn.RuntimeSnapshotID
			err, ok := checked[id]
			if !ok {
				err = checkHost(ctx, id)
				checked[id] = err
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if err != nil {
				candidate.Phase = "unavailable"
				candidate.Reason = err.Error()
				if errors.Is(err, tool.ErrSnapshotHostChanged) {
					candidate.Reason = "requires the original GSEA executable"
				}
			}
		}
		candidates = append(candidates, candidate)
	}
	return candidates, nil
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
	return conversation.resumeRecordedTurn(ctx, *latest, sink, false)
}

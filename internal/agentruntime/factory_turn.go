package agentruntime

import (
	"context"
	"errors"
	"fmt"

	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

// NewTurn captures one immutable runtime snapshot and constructs Eino's Agent
// and Runner around it.
func (f *Factory) NewTurn(ctx context.Context, sessionID session.ID) (*Turn, error) {
	return f.NewTurnWithAdmission(ctx, sessionID, "")
}

func (f *Factory) NewTurnWithAdmission(ctx context.Context, sessionID session.ID, admissionToken string) (*Turn, error) {
	selected, err := f.withActiveStrategy(ctx)
	if err != nil {
		return nil, err
	}
	if selected != f {
		return selected.NewTurnWithAdmission(ctx, sessionID, admissionToken)
	}
	if f.profile != nil && sessionID != f.profileSession {
		return nil, &ProfileConstraintError{Reason: "session differs from bound Agent"}
	}
	if sessionID == "" {
		return nil, errors.New("turn requires a session ID")
	}
	// Earlier turns have finished; settle the messages they held so the new
	// turn can claim those that were never committed.
	if err := f.settleBackground(ctx, sessionID, true); err != nil {
		return nil, err
	}
	snapshot := f.registry.Snapshot()
	runtimeSnapshotID, err := snapshot.ID()
	if err != nil {
		return nil, err
	}
	turnID, err := randomID()
	if err != nil {
		return nil, err
	}
	if err := f.registry.RetainSnapshot(ctx, snapshot, tool.SnapshotReference{Kind: "turn", Scope: string(sessionID), ID: turnID}); err != nil {
		return nil, fmt.Errorf("retain turn runtime: %w", err)
	}
	current, err := f.events.CurrentSequence(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("read session before turn: %w", err)
	}
	if current == 0 {
		if _, err := f.events.Append(ctx, sessionID, 0, session.PendingEvent{
			Type:              session.EventSessionCreated,
			CorrelationID:     turnID,
			RuntimeSnapshotID: runtimeSnapshotID,
			Payload: session.MustJSONPayload(map[string]any{
				"agent": f.config.Name,
			}),
		}); err != nil {
			return nil, fmt.Errorf("create Agent session: %w", err)
		}
	}
	return f.buildTurnWithAdmission(ctx, sessionID, turnID, runtimeSnapshotID, snapshot, 0, admissionToken)
}

// ResumeTurn reconstructs the exact turn-scoped agent and resumes its Eino
// execution from the persisted checkpoint identified by the turn ID.
func (f *Factory) ResumeTurn(ctx context.Context, sessionID session.ID, turnID string) (*Turn, error) {
	selected, err := f.withTurnStrategy(ctx, sessionID, turnID)
	if err != nil {
		return nil, err
	}
	if selected != f {
		return selected.ResumeTurn(ctx, sessionID, turnID)
	}
	if f.profile != nil && sessionID != f.profileSession {
		return nil, &ProfileConstraintError{Reason: "session differs from bound Agent"}
	}
	if sessionID == "" || turnID == "" {
		return nil, errors.New("resume requires a session ID and turn ID")
	}
	if err := f.CheckTurnRecovery(ctx, sessionID, turnID); err != nil {
		return nil, err
	}
	checkpoint := checkpointID(sessionID, turnID)
	hasCheckpoint := false
	var checkpointBytes []byte
	if f.checkpoints != nil {
		checkpointBytes, hasCheckpoint, err = f.checkpoints.Get(ctx, checkpoint)
		if err != nil {
			return nil, fmt.Errorf("inspect turn checkpoint: %w", err)
		}
	}
	var journal *journalReplay
	var prepared *modelReceipt
	if hasCheckpoint {
		prepared, err = f.loadPreparedRequest(ctx, sessionID, turnID)
		if err != nil {
			return nil, fmt.Errorf("restore checkpoint prompt view: %w", err)
		}
	}
	if !hasCheckpoint || prepared != nil {
		journal, err = f.loadTurnJournal(ctx, sessionID, turnID)
		if err != nil {
			return nil, fmt.Errorf("load turn journal: %w", err)
		}
		if journal == nil {
			return nil, fmt.Errorf("turn checkpoint %q does not exist and no replay journal is available", checkpoint)
		}
		if journal.Completed {
			return nil, errors.New("turn is already completed")
		}
		if hasCheckpoint {
			offset, matched, err := f.checkpointReplayOffset(ctx, sessionID, turnID, checkpointBytes)
			if err != nil {
				return nil, fmt.Errorf("restore checkpoint journal offset: %w", err)
			}
			if matched {
				if offset > len(journal.Responses) {
					return nil, errors.New("checkpoint journal offset exceeds confirmed model responses")
				}
				journal.Next = offset
			} else {
				// A process may die between Eino saving new checkpoint bytes
				// and the host saving their replay offset. Reconstruct from the
				// original journal instead of trusting a stale checkpoint marker.
				hasCheckpoint = false
			}
		}
	}
	runtimeSnapshotID, err := f.snapshotIDForTurn(ctx, sessionID, turnID)
	if err != nil {
		return nil, err
	}
	snapshot, err := f.registry.ResolveSnapshot(ctx, runtimeSnapshotID)
	if err != nil {
		return nil, fmt.Errorf("restore turn %q: %w", turnID, err)
	}
	if err := f.validateTurnModel(ctx, sessionID, turnID); err != nil {
		return nil, err
	}
	if err := f.registry.RetainSnapshot(ctx, snapshot, tool.SnapshotReference{Kind: "turn", Scope: string(sessionID), ID: turnID}); err != nil {
		return nil, fmt.Errorf("retain resumed turn runtime: %w", err)
	}
	requestSequence, err := f.modelRequestSequence(ctx, sessionID, turnID)
	if err != nil {
		return nil, err
	}
	goalMode, err := f.goalModeForTurn(ctx, sessionID, turnID)
	if err != nil {
		return nil, err
	}
	resumed := *f
	resumed.goalMode.Enabled = goalMode
	resumed.goalMode.Activation, err = f.activationForTurn(ctx, sessionID, turnID)
	if err != nil {
		return nil, err
	}
	turn, err := resumed.buildTurnWithAdmission(ctx, sessionID, turnID, runtimeSnapshotID, snapshot, requestSequence, "")
	if err != nil {
		return nil, err
	}
	turn.recordedModel.replay = journal
	if !hasCheckpoint {
		turn.journal = journal
	}
	if hasCheckpoint {
		if prepared != nil {
			if err := turn.recordedModel.restoreModelReceipt(*prepared, nil); err != nil {
				return nil, err
			}
		}
	}
	return turn, nil
}

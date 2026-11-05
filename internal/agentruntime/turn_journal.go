package agentruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

// The journal reconstructs Eino's execution by replaying confirmed model
// responses and Pipeline receipts. It is not another agent loop. An unfinished
// model request may be generated again; an unknown tool effect may not.
type turnJournal struct {
	Version  int               `json:"version"`
	Input    []*schema.Message `json:"input"`
	Prepared bool              `json:"prepared"`
	Receipt  *modelReceipt     `json:"receipt,omitempty"`
}

type modelReceipt struct {
	Version   int               `json:"version"`
	RawDigest string            `json:"raw_digest"`
	Covered   int               `json:"covered"`
	Digest    [32]byte          `json:"digest"`
	Input     []*schema.Message `json:"input"`
	Response  *schema.Message   `json:"response"`
}

type journalReplay struct {
	turnJournal
	Responses    []modelReceipt
	Next         int
	Completed    bool
	Pending      *modelReceipt
	PendingAfter int
}

func (f *Factory) loadTurnJournal(ctx context.Context, id session.ID, turnID string) (*journalReplay, error) {
	var replay *journalReplay
	err := session.VisitEvents(ctx, f.events, id, func(event session.Event) error {
		if event.CorrelationID != turnID {
			return nil
		}
		switch event.Type {
		case session.EventTurnStarted:
			var payload struct {
				Journal *turnJournal `json:"journal"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return err
			}
			if payload.Journal == nil {
				return nil
			} // Legacy turns need Eino checkpoints.
			if payload.Journal.Version != 1 || len(payload.Journal.Input) == 0 {
				return errors.New("unsupported or empty turn journal")
			}
			replay = &journalReplay{turnJournal: *payload.Journal}
		case session.EventTurnPrepared:
			if replay == nil {
				return nil
			}
			var prepared turnJournal
			if err := json.Unmarshal(event.Payload, &prepared); err != nil {
				return err
			}
			if !prepared.Prepared {
				return errors.New("invalid prepared turn journal")
			}
			switch prepared.Version {
			case 1:
				// Legacy prepared input was already processed by Eino. Never
				// pass it back to Runner.Run, which adds its Instruction again.
				if len(prepared.Input) == 0 {
					return errors.New("invalid legacy prepared turn journal")
				}
			case 2:
				if err := validateModelReceipt(prepared.Receipt); err != nil {
					return err
				}
				replay.Pending = prepared.Receipt
				replay.PendingAfter = len(replay.Responses)
			default:
				return errors.New("unsupported prepared turn journal")
			}
		case session.EventAssistantMessage:
			if replay == nil {
				return nil
			}
			var payload struct {
				Receipt *modelReceipt `json:"receipt"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return err
			}
			if payload.Receipt == nil || payload.Receipt.Response == nil || payload.Receipt.RawDigest == "" {
				return errors.New("turn contains a model response without a recovery receipt")
			}
			if err := validateModelReceipt(payload.Receipt); err != nil {
				return err
			}
			replay.Responses = append(replay.Responses, *payload.Receipt)
		case session.EventTurnCompleted:
			if replay != nil {
				replay.Completed = true
			}
		}
		return nil
	})
	return replay, err
}

// Eino checkpoints already own the loop position. They only need the latest
// host prompt view restored, rather than replaying any earlier model decision.
func (f *Factory) loadPreparedRequest(ctx context.Context, id session.ID, turnID string) (*modelReceipt, error) {
	var receipt *modelReceipt
	err := session.VisitEvents(ctx, f.events, id, func(event session.Event) error {
		if event.CorrelationID != turnID || event.Type != session.EventTurnPrepared {
			return nil
		}
		var prepared turnJournal
		if err := json.Unmarshal(event.Payload, &prepared); err != nil {
			return err
		}
		if prepared.Version != 2 {
			return nil // Legacy checkpoints did not persist this host view.
		}
		if err := validateModelReceipt(prepared.Receipt); err != nil {
			return err
		}
		receipt = prepared.Receipt
		return nil
	})
	return receipt, err
}

func checkpointJournalDigest(checkpoint []byte) string {
	digest := sha256.Sum256(checkpoint)
	return hex.EncodeToString(digest[:])
}

// A checkpoint may precede later confirmed work. Bind its replay offset to
// the opaque checkpoint bytes, so replacing a checkpoint without committing
// its marker cannot accidentally reuse the offset of an older interruption.
func (f *Factory) checkpointReplayOffset(ctx context.Context, id session.ID, turnID string, checkpoint []byte) (int, bool, error) {
	offset, found := 0, false
	digest := checkpointJournalDigest(checkpoint)
	err := session.VisitEvents(ctx, f.events, id, func(event session.Event) error {
		if event.CorrelationID != turnID || event.Type != session.EventCheckpointCreated {
			return nil
		}
		var marker struct {
			Digest string `json:"checkpoint_sha256"`
			Offset *int   `json:"journal_response_count"`
		}
		if err := json.Unmarshal(event.Payload, &marker); err != nil {
			return err
		}
		if marker.Digest == digest && marker.Offset != nil && *marker.Offset >= 0 {
			offset, found = *marker.Offset, true
		}
		return nil
	})
	return offset, found, err
}

func (t *Turn) recordJournalCheckpoint(ctx context.Context) error {
	if t.checkpoints == nil || t.recordedModel == nil || t.recordedModel.factory == nil {
		return errors.New("Agent wait has no durable checkpoint services")
	}
	durable, cancel := durabilityContext(ctx)
	defer cancel()
	checkpoint, found, err := t.checkpoints.Get(durable, t.checkpointID)
	if err != nil {
		return fmt.Errorf("read Agent wait checkpoint: %w", err)
	}
	if !found {
		return errors.New("Agent wait checkpoint was not persisted")
	}
	journal, err := t.recordedModel.factory.loadTurnJournal(durable, t.sessionID, t.turnID)
	if err != nil {
		return fmt.Errorf("load Agent wait journal: %w", err)
	}
	if journal == nil {
		return errors.New("Agent wait journal was not persisted")
	}
	_, err = t.events.Append(durable, t.sessionID, session.AnySequence, session.PendingEvent{
		Type: session.EventCheckpointCreated, CorrelationID: t.turnID, RuntimeSnapshotID: t.runtimeSnapshotID,
		Payload: session.MustJSONPayload(map[string]any{
			"turn_id": t.turnID, "checkpoint_id": t.checkpointID,
			"checkpoint_sha256": checkpointJournalDigest(checkpoint), "journal_response_count": len(journal.Responses),
		}),
	})
	return err
}

func validateModelReceipt(receipt *modelReceipt) error {
	if receipt == nil || receipt.Version != 2 || receipt.RawDigest == "" || receipt.Covered <= 0 || len(receipt.Input) == 0 {
		return errors.New("turn contains an incomplete or unsupported model recovery receipt")
	}
	return nil
}

func rawRequestDigest(input []*schema.Message) string {
	digest := requestPrefixDigest(input)
	return hex.EncodeToString(digest[:])
}

func (m *recordingModel) replayResponse(input []*schema.Message, options []model.Option) (*schema.Message, bool, error) {
	replay := m.replay
	if replay == nil || replay.Next >= len(replay.Responses) {
		return nil, false, nil
	}
	receipt := replay.Responses[replay.Next]
	if receipt.RawDigest != rawRequestDigest(input) {
		return nil, true, fmt.Errorf("turn journal diverged at model response %d; refusing replacement execution", replay.Next+1)
	}
	response, err := cloneMessage(receipt.Response)
	if err != nil {
		return nil, true, err
	}
	if err := m.restoreModelReceipt(receipt, options); err != nil {
		return nil, true, err
	}
	replay.Next++
	return response, true, nil
}

// restoreModelReceipt restores the effective prompt independently of Eino's
// raw history. Confirmed decisions must not claim new mailbox input, run a
// summarizer again, or spend another model reservation during reconstruction.
func (m *recordingModel) restoreModelReceipt(receipt modelReceipt, options []model.Option) error {
	stable, err := cloneJournalInput(receipt.Input)
	if err != nil {
		return err
	}
	m.contextState.mu.Lock()
	m.contextState.covered, m.contextState.digest = receipt.Covered, receipt.Digest
	m.contextState.prepared = stable
	m.contextState.mu.Unlock()
	m.receipt = receipt
	return m.captureRequest(stable, options)
}

func cloneJournalInput(input []*schema.Message) ([]*schema.Message, error) {
	stable := make([]*schema.Message, len(input))
	for index, message := range input {
		if message == nil {
			continue
		}
		var err error
		stable[index], err = cloneMessage(message)
		if err != nil {
			return nil, err
		}
	}
	return stable, nil
}

package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

// TurnRecord is a read-only projection. A checkpoint's presence is not proof
// of compatibility; ResumeTurn still validates model, profile and registry.
type TurnRecord struct {
	TurnID                  string
	RunID                   orchestration.RunID
	ExecutionAdmissionToken string
	LegacyAdmissionRecord   bool
	Completed               bool
	Abandoned               bool
	Archived                bool
	LoopStopped             bool
	HasCheckpoint           bool
	HasJournal              bool
	ForegroundSequence      uint64
	Background              bool
	HasUserInput            bool
	OpeningPreview          string
	RuntimeSnapshotID       string
	StartedSequence         uint64
	CompletedSequence       uint64
	openedWithPriorWork     bool
}

// RequiresExecutionRecovery prevents an old admitted turn from being silently
// resumed as ordinary chat.
func (record TurnRecord) RequiresExecutionRecovery() bool {
	return record.LegacyAdmissionRecord || record.ExecutionAdmissionToken != ""
}

// IsForegroundRequest identifies an opening user request, including starts
// that predate the durable foreground marker.
func (record TurnRecord) IsForegroundRequest() bool {
	return !record.Background && record.RunID == "" && record.HasUserInput
}

func (f *Factory) CheckTurnRecovery(ctx context.Context, id session.ID, turnID string) error {
	if f.profile != nil && id != f.profileSession {
		return errors.New("recovery session differs from bound Agent")
	}
	return session.CheckRecovery(ctx, f.events, id, turnID)
}

func (f *Factory) TurnRecords(ctx context.Context, id session.ID) ([]TurnRecord, error) {
	if f.profile != nil && id != f.profileSession {
		return nil, errors.New("recovery session differs from bound Agent")
	}
	return LoadTurnRecords(ctx, f.events, f.checkpoints, id)
}

// CheckSnapshotHost performs the read-only host check used by recovery listings.
func (f *Factory) CheckSnapshotHost(ctx context.Context, snapshotID string) error {
	return f.registry.CheckSnapshotHost(ctx, snapshotID)
}

// SelectForegroundTurn persists the host's decision to resume an existing turn.
func (f *Factory) SelectForegroundTurn(ctx context.Context, id session.ID, turnID string) error {
	_, err := f.events.Append(ctx, id, session.AnySequence, session.PendingEvent{
		Type: session.EventTurnForegroundSelected, CorrelationID: turnID, Payload: session.MustJSONPayload(struct{}{}),
	})
	return err
}

// LoadTurnRecords needs only read ports, not a model or executable registry.
func LoadTurnRecords(ctx context.Context, events session.EventStore, checkpoints session.CheckpointStore, id session.ID) ([]TurnRecord, error) {
	var records []TurnRecord
	indices := make(map[string]int)
	err := session.VisitEvents(ctx, events, id, func(event session.Event) error {
		if event.Type == session.EventTurnStarted {
			if _, exists := indices[event.CorrelationID]; exists {
				return errors.New("duplicate turn start in recovery history")
			}
			var metadata struct {
				RunID           orchestration.RunID `json:"run_id"`
				Journal         *turnJournal        `json:"journal"`
				LegacyAdmission bool                `json:"goal_mode"`
				AdmissionToken  string              `json:"admission_token"`
				Input           string              `json:"input"`
				Foreground      bool                `json:"foreground"`
			}
			if err := json.Unmarshal(event.Payload, &metadata); err != nil {
				return err
			}
			indices[event.CorrelationID] = len(records)
			record := TurnRecord{
				TurnID: event.CorrelationID, RunID: metadata.RunID, LegacyAdmissionRecord: metadata.LegacyAdmission,
				ExecutionAdmissionToken: metadata.AdmissionToken, Background: metadata.Input == TurnInputBackground,
				RuntimeSnapshotID: event.RuntimeSnapshotID,
				StartedSequence:   event.Sequence,
				HasJournal:        metadata.Journal != nil && metadata.Journal.Version == 1 && len(metadata.Journal.Input) > 0,
			}
			if metadata.Foreground {
				record.ForegroundSequence = event.Sequence
			}
			if record.HasJournal && len(metadata.Journal.Input) >= 2 {
				// runInnerTurn freezes the host's prior-work message immediately
				// before the actual user input. A history-only opening has none.
				prior := metadata.Journal.Input[len(metadata.Journal.Input)-2]
				record.openedWithPriorWork = prior != nil && prior.Role == schema.User &&
					strings.HasPrefix(prior.Content, interruptedContextHeader+"\n")
			}
			records = append(records, record)
		} else if index, exists := indices[event.CorrelationID]; exists {
			record := &records[index]
			switch event.Type {
			case session.EventTurnForegroundSelected:
				record.ForegroundSequence = event.Sequence
			case session.EventUserMessage:
				var payload struct {
					Content string `json:"content"`
				}
				if err := json.Unmarshal(event.Payload, &payload); err != nil {
					return err
				}
				record.HasUserInput = true
				if input := strings.TrimSpace(payload.Content); input != "" {
					preview := []rune(strings.Join(strings.Fields(input), " "))
					if len(preview) > 80 {
						preview = append(preview[:79], '…')
					}
					record.OpeningPreview = string(preview)
				}
			case session.EventTurnCompleted:
				record.Completed = true
				record.CompletedSequence = event.Sequence
			case session.EventTurnAbandoned:
				record.Abandoned = true
			case session.EventTurnArchived:
				record.Archived = true
			case session.EventTurnFailed:
				code, err := event.TurnFailureCode()
				if err != nil {
					return err
				}
				record.LoopStopped = record.LoopStopped || code == session.TurnFailureToolCycle
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if checkpoints != nil {
		for index := range records {
			if records[index].Completed || records[index].Abandoned || records[index].Archived || records[index].LoopStopped {
				continue
			}
			_, records[index].HasCheckpoint, err = checkpoints.Get(ctx, checkpointID(id, records[index].TurnID))
			if err != nil {
				return nil, err
			}
		}
	}
	return records, nil
}

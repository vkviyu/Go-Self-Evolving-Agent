package agentruntime

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

// TurnRecord is a read-only projection. A checkpoint's presence is not proof
// of compatibility; ResumeTurn still validates model, profile and registry.
type TurnRecord struct {
	TurnID         string
	RunID          orchestration.RunID
	GoalMode       bool
	GoalActivation agentgoal.Activation
	AdmissionToken string
	Completed      bool
	Abandoned      bool
	Archived       bool
	HasCheckpoint  bool
	HasJournal     bool
}

// RequiresGoalBinding prevents a persisted Goal-mode turn from being resumed
// as ordinary chat when a crash interrupted round registration.
func (record TurnRecord) RequiresGoalBinding() bool {
	return record.GoalMode && (record.GoalActivation == agentgoal.ActivationAlways || record.AdmissionToken != "")
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
				RunID          orchestration.RunID  `json:"run_id"`
				Journal        *turnJournal         `json:"journal"`
				GoalMode       bool                 `json:"goal_mode"`
				GoalActivation agentgoal.Activation `json:"goal_activation"`
				AdmissionToken string               `json:"admission_token"`
			}
			if err := json.Unmarshal(event.Payload, &metadata); err != nil {
				return err
			}
			indices[event.CorrelationID] = len(records)
			if metadata.GoalMode && metadata.GoalActivation == "" {
				metadata.GoalActivation = agentgoal.ActivationAlways
			}
			records = append(records, TurnRecord{TurnID: event.CorrelationID, RunID: metadata.RunID, GoalMode: metadata.GoalMode, GoalActivation: metadata.GoalActivation, AdmissionToken: metadata.AdmissionToken, HasJournal: metadata.Journal != nil && metadata.Journal.Version == 1 && len(metadata.Journal.Input) > 0})
		} else if index, exists := indices[event.CorrelationID]; exists {
			switch event.Type {
			case session.EventTurnCompleted:
				records[index].Completed = true
			case session.EventTurnAbandoned:
				records[index].Abandoned = true
			case session.EventTurnArchived:
				records[index].Archived = true
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if checkpoints != nil {
		for index := range records {
			if records[index].Completed || records[index].Abandoned || records[index].Archived {
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

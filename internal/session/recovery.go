package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrRecoveryAbandoned  = errors.New("recovery was explicitly abandoned")
	ErrRecoveryDependency = errors.New("recovery still has dependent work")
	ErrRecoveryArchived   = errors.New("recovery was explicitly archived; history remains readable")
)

// AbandonRequest closes recovery, not its already-performed side effects.
// It is accepted only by the offline owner, never by a model-callable tool.
type AbandonRequest struct {
	SessionID       ID
	TurnID          string
	GoalID          string
	Reason          string
	EffectsReviewed bool
}

func (request AbandonRequest) Validate() error {
	if request.SessionID == "" || (request.TurnID == "") == (request.GoalID == "") {
		return errors.New("abandon requires a session and exactly one turn or goal")
	}
	if !request.EffectsReviewed {
		return errors.New("abandon requires explicit acknowledgement that prior effects were inspected and are not undone")
	}
	if strings.TrimSpace(request.Reason) == "" || len(request.Reason) > 2048 {
		return errors.New("abandon reason is required and must not exceed 2048 bytes")
	}
	if len(request.SessionID) > 4096 || len(request.TurnID) > 4096 || len(request.GoalID) > 4096 {
		return errors.New("abandon identity is too long")
	}
	return nil
}

type Abandonment struct {
	SessionID          ID        `json:"session_id"`
	GoalID             string    `json:"goal_id,omitempty"`
	TurnIDs            []string  `json:"turn_ids"`
	Reason             string    `json:"reason"`
	ReleasedReferences int       `json:"released_references"`
	AbandonedAt        time.Time `json:"abandoned_at"`
	EffectsReverted    bool      `json:"effects_reverted"`
}

// CheckRecovery refuses a tombstoned turn even when its checkpoint and old
// snapshot are still available. Evidence remains readable through EventStore.
func CheckRecovery(ctx context.Context, events EventStore, id ID, turnID string) error {
	check := func(event Event) error {
		if event.CorrelationID == turnID && event.Type == EventTurnArchived {
			return fmt.Errorf("%w: %s", ErrRecoveryArchived, turnID)
		}
		if event.CorrelationID == turnID && event.Type == EventTurnAbandoned {
			return fmt.Errorf("%w: %s", ErrRecoveryAbandoned, turnID)
		}
		return nil
	}
	if reader, ok := events.(CorrelatedEventStore); ok {
		items, err := reader.LoadByCorrelation(ctx, id, turnID)
		if err != nil {
			return err
		}
		for _, event := range items {
			if err := check(event); err != nil {
				return err
			}
		}
		return nil
	}
	return VisitEvents(ctx, events, id, check)
}

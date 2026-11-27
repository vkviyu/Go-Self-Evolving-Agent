package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrRecoveryAbandoned  = errors.New("recovery was explicitly abandoned")
	ErrRecoveryDependency = errors.New("recovery still has dependent work")
	ErrRecoveryArchived   = errors.New("recovery was explicitly archived; history remains readable")
	ErrRecoveryToolCycle  = errors.New("turn stopped after repeated tool calls returned the same results; start a new turn to change approach")
)

const TurnFailureToolCycle = "repeated_tool_cycle"

// TurnFailureCode reads the optional structured failure code from a turn event.
func (event Event) TurnFailureCode() (string, error) {
	if event.Type != EventTurnFailed {
		return "", nil
	}
	var payload struct {
		Code string `json:"failure_code"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return "", fmt.Errorf("read turn failure code: %w", err)
	}
	return payload.Code, nil
}

// AbandonRequest closes recovery, not its already-performed side effects.
// It is accepted only by the offline owner, never by a model-callable tool.
type AbandonRequest struct {
	SessionID       ID
	TurnID          string
	Reason          string
	EffectsReviewed bool
}

// Validate requires the target session and turn and explicit review of possible side
// effects before abandonment.
func (request AbandonRequest) Validate() error {
	if request.SessionID == "" || request.TurnID == "" {
		return errors.New("abandon requires a session and turn")
	}
	if !request.EffectsReviewed {
		return errors.New("abandon requires explicit acknowledgement that prior effects were inspected and are not undone")
	}
	if strings.TrimSpace(request.Reason) == "" || len(request.Reason) > 2048 {
		return errors.New("abandon reason is required and must not exceed 2048 bytes")
	}
	if len(request.SessionID) > 4096 || len(request.TurnID) > 4096 {
		return errors.New("abandon identity is too long")
	}
	return nil
}

type Abandonment struct {
	SessionID          ID        `json:"session_id"`
	TurnIDs            []string  `json:"turn_ids"`
	Reason             string    `json:"reason"`
	ReleasedReferences int       `json:"released_references"`
	AbandonedAt        time.Time `json:"abandoned_at"`
	EffectsReverted    bool      `json:"effects_reverted"`
}

// CheckRecovery refuses a tombstoned turn even when its checkpoint and old
// snapshot are still available. Evidence remains readable through EventStore.
func CheckRecovery(ctx context.Context, events EventStore, id ID, turnID string) error {
	loopStopped := false
	check := func(event Event) error {
		if event.CorrelationID != turnID {
			return nil
		}
		switch event.Type {
		case EventTurnArchived:
			return fmt.Errorf("%w: %s", ErrRecoveryArchived, turnID)
		case EventTurnAbandoned:
			return fmt.Errorf("%w: %s", ErrRecoveryAbandoned, turnID)
		case EventTurnFailed:
			code, err := event.TurnFailureCode()
			if err != nil {
				return err
			}
			loopStopped = loopStopped || code == TurnFailureToolCycle
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
	} else if err := VisitEvents(ctx, events, id, check); err != nil {
		return err
	}
	if loopStopped {
		return fmt.Errorf("%w (%s)", ErrRecoveryToolCycle, turnID)
	}
	return nil
}

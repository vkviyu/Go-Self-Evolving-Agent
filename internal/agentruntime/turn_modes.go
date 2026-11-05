package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func (f *Factory) activationForTurn(ctx context.Context, sessionID session.ID, turnID string) (agentgoal.Activation, error) {
	activation := agentgoal.ActivationAlways // legacy goal-enabled checkpoints
	err := session.VisitEvents(ctx, f.events, sessionID, func(event session.Event) error {
		if event.Type != session.EventTurnStarted || event.CorrelationID != turnID {
			return nil
		}
		var payload struct {
			Activation agentgoal.Activation `json:"goal_activation"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return err
		}
		if payload.Activation != "" {
			activation = payload.Activation
		}
		return nil
	})
	return activation, err
}

func (f *Factory) AutoAdmissionForTurn(ctx context.Context, sessionID session.ID, turnID string) (bool, error) {
	enabled, err := f.goalModeForTurn(ctx, sessionID, turnID)
	if err != nil || !enabled {
		return false, err
	}
	activation, err := f.activationForTurn(ctx, sessionID, turnID)
	return activation == agentgoal.ActivationAuto, err
}

// AdmissionRequestedForTurn restores the fail-closed boundary even when a
// checkpoint predates successful goal registration. Use structured events,
// never model prose or matching human-readable error strings.
func (f *Factory) AdmissionRequestedForTurn(ctx context.Context, sessionID session.ID, turnID string) (bool, error) {
	requested := false
	err := session.VisitEvents(ctx, f.events, sessionID, func(event session.Event) error {
		if event.Type != session.EventToolProposed && event.Type != session.EventToolFailed {
			return nil
		}
		var payload struct {
			TurnID    string         `json:"turn_id"`
			Tool      string         `json:"tool"`
			ErrorCode string         `json:"error_code"`
			RiskClass tool.RiskClass `json:"risk_class"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return err
		}
		if payload.TurnID == turnID && (payload.Tool == agentgoal.StartToolName || payload.Tool == agentgoal.ReportToolName || payload.ErrorCode == agentgoal.AdmissionRequiredCode || payload.RiskClass == tool.RiskWrite) {
			requested = true
		}
		return nil
	})
	return requested, err
}

// TurnInput restores trusted admission input and the original wall-time origin.
func (f *Factory) TurnInput(ctx context.Context, sessionID session.ID, turnID string) (string, time.Time, error) {
	var input string
	var started time.Time
	err := session.VisitEvents(ctx, f.events, sessionID, func(event session.Event) error {
		if event.CorrelationID != turnID {
			return nil
		}
		if event.Type == session.EventTurnStarted {
			started = event.OccurredAt
		}
		if event.Type == session.EventUserMessage {
			var payload struct {
				Content string `json:"content"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return err
			}
			input = payload.Content
		}
		return nil
	})
	if err == nil && (input == "" || started.IsZero()) {
		err = fmt.Errorf("trusted input for turn %q is unavailable", turnID)
	}
	return input, started, err
}

func (f *Factory) goalModeForTurn(ctx context.Context, sessionID session.ID, turnID string) (bool, error) {
	var mode *bool
	err := session.VisitEvents(ctx, f.events, sessionID, func(event session.Event) error {
		if event.Type == session.EventTurnStarted && event.CorrelationID == turnID {
			var payload struct {
				GoalMode *bool `json:"goal_mode"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return fmt.Errorf("decode turn mode: %w", err)
			}
			mode = payload.GoalMode
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	if mode != nil {
		return *mode, nil
	}
	// Legacy checkpoints predate per-turn activation and used the factory mode.
	return f.goalMode.Enabled, nil
}

// CountCompletedUserTurns counts direct turns plus terminal goals. Counting
// goals alone loses greetings after restart; counting all inner turns instead
// counts rejected goal candidates as additional user messages. Wake turns
// opened by Agent messages are not user turns.
func (f *Factory) CountCompletedUserTurns(ctx context.Context, sessionID session.ID) (int, error) {
	modes := make(map[string]*bool)
	completed := make(map[string]bool)
	background := make(map[string]bool)
	err := session.VisitEvents(ctx, f.events, sessionID, func(event session.Event) error {
		switch event.Type {
		case session.EventTurnStarted:
			var payload struct {
				GoalMode *bool  `json:"goal_mode"`
				Input    string `json:"input"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return fmt.Errorf("decode turn mode: %w", err)
			}
			modes[event.CorrelationID] = payload.GoalMode
			background[event.CorrelationID] = payload.Input == TurnInputBackground
		case session.EventTurnCompleted:
			completed[event.CorrelationID] = true
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	count := 0
	for turnID := range completed {
		if background[turnID] {
			continue
		}
		if f.goalStore != nil {
			// Tool availability is no longer goal admission. The durable mapping
			// is authoritative for auto, always, and legacy turns alike.
			_, _, found, err := f.goalStore.GoalForTurn(ctx, sessionID, turnID)
			if err != nil {
				return 0, fmt.Errorf("resolve legacy turn goal: %w", err)
			}
			if found {
				continue
			}
		} else if mode := modes[turnID]; mode != nil && *mode {
			continue
		}
		count++
	}
	if f.goalStore != nil {
		goals, err := f.goalStore.CountGoals(ctx, sessionID)
		if err != nil {
			return 0, fmt.Errorf("count completed goals: %w", err)
		}
		count += goals
	}
	return count, nil
}

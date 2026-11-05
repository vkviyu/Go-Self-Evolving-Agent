package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

// snapshotIDForTurn returns the runtime snapshot recorded by turn.started.
func (f *Factory) snapshotIDForTurn(ctx context.Context, sessionID session.ID, turnID string) (string, error) {
	snapshotID, found := "", false
	err := session.VisitEvents(ctx, f.events, sessionID, func(event session.Event) error {
		if event.Type != session.EventTurnStarted || event.CorrelationID != turnID {
			return nil
		}
		if event.RuntimeSnapshotID == "" {
			return fmt.Errorf("turn %q has no runtime snapshot", turnID)
		}
		snapshotID, found = event.RuntimeSnapshotID, true
		return session.ErrStopVisit
	})
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("turn %q metadata does not exist", turnID)
	}
	return snapshotID, nil
}

// modelRequestSequence reconstructs the process-local request counter from
// durable model.requested events and reservations. Resuming after a restart must
// continue at the next request ID, otherwise usage idempotency keys can be
// reused for a different provider call.
func (f *Factory) modelRequestSequence(ctx context.Context, sessionID session.ID, turnID string) (uint64, error) {
	var highest uint64
	err := session.VisitEvents(ctx, f.events, sessionID, func(event session.Event) error {
		if event.Type != session.EventModelRequested || event.CorrelationID != turnID {
			return nil
		}
		var payload struct {
			RequestID       string `json:"request_id"`
			RequestSequence uint64 `json:"request_sequence"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode model request sequence: %w", err)
		}
		sequence := payload.RequestSequence
		if sequence == 0 && payload.RequestID != "" {
			if separator := strings.LastIndexByte(payload.RequestID, ':'); separator >= 0 {
				sequence, _ = strconv.ParseUint(payload.RequestID[separator+1:], 10, 64)
			}
		}
		highest = max(highest, sequence)
		return nil
	})
	if err != nil {
		return 0, err
	}
	// Reserve can commit before model.requested is appended. Include even
	// refunded reservation identities so restart never reuses that number.
	if ledger, ok := f.config.UsageLedger.(tokenusage.DurableReservationLedger); ok {
		reserved, err := ledger.ModelRequestSequence(ctx, f.config.ActorScope.TeamID, turnID)
		if err != nil {
			return 0, err
		}
		highest = max(highest, reserved)
	}
	return highest, nil
}

// validateTurnModel prevents a checkpoint from silently switching provider or
// model after a process restart. Events written before model identity was
// recorded are accepted; every new Turn carries the identity in turn.started.
func (f *Factory) validateTurnModel(ctx context.Context, sessionID session.ID, turnID string) error {
	return session.VisitEvents(ctx, f.events, sessionID, func(event session.Event) error {
		if event.Type != session.EventTurnStarted || event.CorrelationID != turnID {
			return nil
		}
		var payload struct {
			ModelProfile      string `json:"model_profile"`
			Provider          string `json:"provider"`
			Model             string `json:"model"`
			ExecutionConfigID string `json:"execution_config_id"`
			RunID             string `json:"run_id"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode turn model snapshot: %w", err)
		}
		if payload.RunID != "" && payload.RunID != string(f.config.ActorScope.RunID) {
			return fmt.Errorf("%w: original Run ID differs from recovery scope", ErrTurnModelChanged)
		}
		if payload.ExecutionConfigID != "" && payload.ExecutionConfigID != f.executionConfigID() {
			return fmt.Errorf("%w: turn instruction, profile or execution limits changed", ErrTurnModelChanged)
		}
		if payload.ModelProfile != "" && payload.ModelProfile != f.config.ModelProfile {
			return fmt.Errorf("%w: turn %q model profile changed from %q to %q", ErrTurnModelChanged, turnID, payload.ModelProfile, f.config.ModelProfile)
		}
		if payload.Provider != "" && payload.Provider != f.config.Provider {
			return fmt.Errorf("%w: turn %q provider changed from %q to %q", ErrTurnModelChanged, turnID, payload.Provider, f.config.Provider)
		}
		if payload.Model != "" && payload.Model != f.config.Model {
			return fmt.Errorf("%w: turn %q model changed from %q to %q", ErrTurnModelChanged, turnID, payload.Model, f.config.Model)
		}
		// Only the first turn.started of the turn records its model identity.
		return session.ErrStopVisit
	})
}

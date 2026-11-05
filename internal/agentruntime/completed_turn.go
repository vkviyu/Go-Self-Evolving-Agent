package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

// CompletedTurn reads the durable Inner Loop outcome, not an Eino checkpoint.
// A completed turn is never executed again just to reconstruct its candidate.
func (f *Factory) CompletedTurn(ctx context.Context, id session.ID, turnID string) (Result, bool, error) {
	selected, err := f.withTurnStrategy(ctx, id, turnID)
	if err != nil {
		return Result{}, false, err
	}
	if selected != f {
		return selected.CompletedTurn(ctx, id, turnID)
	}
	if err := f.CheckTurnRecovery(ctx, id, turnID); err != nil {
		return Result{}, false, err
	}
	var result Result
	err = session.VisitEvents(ctx, f.events, id, func(event session.Event) error {
		if event.Type != session.EventTurnCompleted || event.CorrelationID != turnID {
			return nil
		}
		var payload struct {
			Content      string        `json:"content"`
			ContextUsage *ContextUsage `json:"context_assessment"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return err
		}
		if payload.Content == "" {
			return errors.New("completed turn has no durable candidate")
		}
		result = Result{TurnID: turnID, RuntimeSnapshotID: event.RuntimeSnapshotID, Final: schema.AssistantMessage(payload.Content, nil), ContextUsage: payload.ContextUsage}
		return nil
	})
	if err != nil || result.Final == nil {
		return Result{}, false, err
	}
	if err := f.validateTurnModel(ctx, id, turnID); err != nil {
		return Result{}, false, err
	}
	identity, err := f.snapshotIDForTurn(ctx, id, turnID)
	if err != nil {
		return Result{}, false, err
	}
	if identity != result.RuntimeSnapshotID {
		return Result{}, false, errors.New("completed turn registry identity mismatch")
	}
	if _, err := f.registry.ResolveSnapshot(ctx, identity); err != nil {
		return Result{}, false, fmt.Errorf("restore assessment runtime: %w", err)
	}
	return result, true, nil
}

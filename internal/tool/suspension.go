package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

type waitSuspensionRecord struct {
	TurnID   string                `json:"turn_id"`
	Tool     string                `json:"tool"`
	Revision string                `json:"revision"`
	Wait     orchestration.WaitSet `json:"wait"`
}

func validateWaitIdentity(call Call, binding Binding, wait orchestration.WaitSet) error {
	if binding.Descriptor.Name != "agent_wait" || binding.Descriptor.RiskClass != RiskRead {
		return fmt.Errorf("tool %s cannot suspend an Agent wait", binding.Descriptor.Name)
	}
	if wait.TeamID != call.Actor.TeamID || wait.AgentID != call.Actor.AgentID || wait.RunID != call.Actor.RunID ||
		wait.ToolCallID != call.CallID || wait.ResumeToken != call.TurnID || wait.IdempotencyKey != call.CallID {
		return fmt.Errorf("Agent wait suspension differs from the invoking run")
	}
	return (&orchestration.WaitSuspendedError{WaitSet: wait}).WaitSpec().Validate()
}

func (p *Pipeline) recordWaitSuspension(ctx context.Context, call Call, binding Binding, suspended *orchestration.WaitSuspendedError) error {
	if err := validateWaitIdentity(call, binding, suspended.WaitSet); err != nil {
		return err
	}
	if err := p.recordAudit(ctx, call, session.PendingEvent{
		Type: session.EventToolSuspended, CorrelationID: call.CallID, RuntimeSnapshotID: call.RuntimeSnapshotID,
		Payload: session.MustJSONPayload(waitSuspensionRecord{TurnID: call.TurnID, Tool: binding.Descriptor.Name, Revision: binding.Descriptor.Revision, Wait: suspended.WaitSet}),
	}); err != nil {
		return fmt.Errorf("record Agent wait suspension: %w", err)
	}
	return nil
}

func (p *Pipeline) restoreWaitIntent(ctx context.Context, call Call, binding Binding) (context.Context, bool, error) {
	if binding.Descriptor.Name != "agent_wait" {
		return ctx, false, nil
	}
	_, supplied := orchestration.WaitIntentFromContext(ctx)
	if wait, ok := orchestration.WaitIntentFromContext(ctx); ok {
		if err := validateWaitIdentity(call, binding, wait); err != nil {
			return nil, false, err
		}
	}
	events, err := correlatedEvents(ctx, p.events, call.SessionID, call.CallID)
	if err != nil {
		return nil, false, fmt.Errorf("restore Agent wait suspension: %w", err)
	}
	resumed := false
	for _, event := range events {
		if event.Type != session.EventToolSuspended {
			continue
		}
		var record waitSuspensionRecord
		if err := json.Unmarshal(event.Payload, &record); err != nil {
			return nil, false, fmt.Errorf("decode Agent wait suspension: %w", err)
		}
		if record.TurnID != call.TurnID {
			continue
		}
		if record.Tool != binding.Descriptor.Name || record.Revision != binding.Descriptor.Revision {
			return nil, false, fmt.Errorf("%w: Agent wait suspension revision", ErrCallConflict)
		}
		if err := validateWaitIdentity(call, binding, record.Wait); err != nil {
			return nil, false, err
		}
		ctx = orchestration.WithWaitIntent(ctx, record.Wait)
		resumed = true
	}
	if supplied && !resumed {
		return nil, false, fmt.Errorf("Agent wait resume has no durable suspension")
	}
	return ctx, resumed, nil
}

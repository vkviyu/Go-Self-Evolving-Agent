package agentruntime

import (
	"context"
	"errors"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

// fail records a recoverable turn boundary even when the request context has
// already been cancelled. Checkpoint availability is part of the durable fact
// so callers can decide whether ResumeTurn is possible.
func (t *Turn) fail(ctx context.Context, cause error) error {
	if ctx.Err() == nil && errors.Is(cause, adk.ErrExceedMaxIterations) {
		cause = &orchestration.TaskAttemptFailure{Code: orchestration.TaskAttemptIterationLimit, Cause: cause}
	}
	recordContext, cancel := durabilityContext(ctx)
	defer cancel()
	checkpointAvailable := false
	if t.checkpoints != nil && t.checkpointID != "" {
		_, checkpointAvailable, _ = t.checkpoints.Get(recordContext, t.checkpointID)
	}
	journalAvailable := false
	if t.recordedModel != nil && t.recordedModel.factory != nil {
		journal, journalErr := t.recordedModel.factory.loadTurnJournal(recordContext, t.sessionID, t.turnID)
		journalAvailable = journalErr == nil && journal != nil
	}
	pending := []session.PendingEvent{{
		Type:              session.EventTurnFailed,
		CorrelationID:     t.turnID,
		RuntimeSnapshotID: t.runtimeSnapshotID,
		Payload: session.MustJSONPayload(map[string]any{
			"turn_id":           t.turnID,
			"error":             cause.Error(),
			"recoverable":       checkpointAvailable || journalAvailable,
			"journal_available": journalAvailable,
			"checkpoint_id":     t.checkpointID,
		}),
	}}
	if checkpointAvailable {
		pending = append(pending, session.PendingEvent{
			Type:              session.EventCheckpointCreated,
			CorrelationID:     t.turnID,
			RuntimeSnapshotID: t.runtimeSnapshotID,
			Payload:           session.MustJSONPayload(map[string]any{"turn_id": t.turnID, "checkpoint_id": t.checkpointID}),
		})
	}
	_, recordErr := t.events.Append(recordContext, t.sessionID, session.AnySequence, pending...)
	if t.strategyFailure != nil && !errors.Is(cause, context.Canceled) {
		recordErr = errors.Join(recordErr, t.strategyFailure(recordContext, cause))
	}
	return errors.Join(cause, recordErr)
}

func durabilityContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
}

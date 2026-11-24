package agentruntime

import (
	"context"
	"errors"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

const durabilityTimeout = 30 * time.Second

// fail records a recoverable turn boundary even when the request context has
// already been cancelled. Checkpoint availability is part of the durable fact
// so callers can decide whether ResumeTurn is possible.
func (t *Turn) fail(ctx context.Context, cause error) error {
	// Eino wraps fatal tool errors in node/stream errors. Translate the typed
	// uncertainty here so every runtime caller gets the same pause semantics,
	// while retaining the entire original chain (including joined errors).
	var unknown *tool.OutcomeUnknownError
	if errors.As(cause, &unknown) && unknown != nil {
		if kind, _ := orchestration.ExecutionStopKindOf(cause); kind != orchestration.ExecutionOutcomeUnknown {
			cause = &orchestration.ExecutionStop{Kind: orchestration.ExecutionOutcomeUnknown, Cause: cause}
		}
	} else if ctx.Err() == nil && errors.Is(cause, adk.ErrExceedMaxIterations) {
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
	payload := map[string]any{
		"turn_id": t.turnID, "error": cause.Error(),
		"recoverable": checkpointAvailable || journalAvailable, "journal_available": journalAvailable,
		"checkpoint_id": t.checkpointID,
	}
	if kind, ok := orchestration.ExecutionStopKindOf(cause); ok {
		payload["stop_kind"] = kind
	} else if errors.Is(cause, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		payload["stop_kind"] = "cancelled"
	}
	if errors.Is(cause, ErrRepeatedToolCycle) {
		payload["failure_code"] = session.TurnFailureToolCycle
		payload["recoverable"] = false
	}
	pending := []session.PendingEvent{{
		Type:              session.EventTurnFailed,
		CorrelationID:     t.turnID,
		RuntimeSnapshotID: t.runtimeSnapshotID,
		Payload:           session.MustJSONPayload(payload),
	}}
	if checkpointAvailable {
		pending = append(pending, session.PendingEvent{
			Type:              session.EventCheckpointCreated,
			CorrelationID:     t.turnID,
			RuntimeSnapshotID: t.runtimeSnapshotID,
			Payload:           session.MustJSONPayload(map[string]any{"turn_id": t.turnID, "checkpoint_id": t.checkpointID}),
		})
	}
	outcome := ExperienceOutcome{Recoverable: payload["recoverable"] == true, Error: cause.Error()}
	if t.recordedModel != nil && t.recordedModel.experienceEnabled() {
		pending = append(pending, t.recordedModel.experiencePending(outcome))
	}
	_, recordErr := t.events.Append(recordContext, t.sessionID, session.AnySequence, pending...)
	cancel()
	persisted := recordErr == nil
	if t.strategyFailure != nil && !errors.Is(cause, context.Canceled) {
		strategyContext, stop := durabilityContext(ctx)
		recordErr = errors.Join(recordErr, t.strategyFailure(strategyContext, cause))
		stop()
	}
	if persisted && t.recordedModel.experienceEnabled() {
		maintenanceContext, stop := durabilityContext(ctx)
		if status := t.recordedModel.maintainExperience(maintenanceContext, outcome); status.Error != "" {
			recordErr = errors.Join(recordErr, errors.New(status.Error))
		}
		stop()
		t.recordedModel.runExperienceRetry(ctx)
	}
	return errors.Join(cause, recordErr)
}

func durabilityContext(parent context.Context) (context.Context, context.CancelFunc) {
	// Native ATE writes may need to start a subprocess and complete the hello
	// handshake before the actual append. Keep durable accounting alive for the
	// same bounded window as the ATE process manager; five seconds was shorter
	// than a cold start and surfaced as a misleading expired client lease.
	return context.WithTimeout(context.WithoutCancel(parent), durabilityTimeout)
}

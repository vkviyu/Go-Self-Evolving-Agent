package agentruntime

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/modeltelemetry"
	"github.com/vkviyu/go-self-evolving-agent/internal/observation"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

// modelInvocationError means provider execution failed but its failure and
// usage were durably accounted for. Storage/ledger failures must not be
// mistaken for an unavailable semantic summarizer and silently downgraded.
type modelInvocationError struct{ cause error }

func (e *modelInvocationError) Error() string { return e.cause.Error() }
func (e *modelInvocationError) Unwrap() error { return e.cause }

// RetryableModelFailure only accepts the accounted model failure itself. An error
// joined with a usage or journal failure must go through explicit recovery.
func RetryableModelFailure(err error) orchestration.ExecutionStopKind {
	for err != nil {
		if invocation, ok := err.(*modelInvocationError); ok {
			kind, _ := orchestration.ExecutionStopKindOf(invocation.cause)
			switch kind {
			case orchestration.ExecutionToolCallFormat:
				return kind
			case orchestration.ExecutionTransport:
				var network net.Error
				if errors.Is(invocation, context.DeadlineExceeded) ||
					(errors.As(invocation, &network) && network.Timeout()) {
					return kind
				}
			}
			return ""
		}
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			causes := joined.Unwrap()
			if len(causes) != 1 {
				return ""
			}
			err = causes[0]
		} else {
			err = errors.Unwrap(err)
		}
	}
	return ""
}

// Once dispatched, absence of reported usage does not mean a free request.
// Charge the bounded reservation conservatively and mark its quality. If the
// ledger is unavailable, leave the reservation pending rather than releasing
// it and hiding consumption. Only releaseUnsent refunds a pre-dispatch failure.
func (m *recordingModel) failRequest(ctx context.Context, requestID string, cause error, streaming bool) error {
	observation.Emit(ctx, observation.Event{Component: "runtime", Name: "model.request.finished", Status: "failed"})
	settlementErr := m.settleUsage(ctx, requestID, nil)
	recordErr := m.recordModelFailure(ctx, requestID, cause, streaming)
	if observation := modeltelemetry.ReadStream(ctx); observation != nil &&
		(observation.Outcome == "response_headers_timeout" || observation.Outcome == "stream_read_timeout" || observation.Outcome == "stream_event_idle_timeout") {
		lastEvent := observation.LastEvent
		if lastEvent == "" {
			lastEvent = "none"
		}
		cause = fmt.Errorf("%w (request %s, received %d bytes, last SSE event %s)", cause, requestID, observation.BodyBytes, lastEvent)
	}
	if settlementErr != nil || recordErr != nil {
		return errors.Join(cause, settlementErr, recordErr)
	}
	if m.purpose == "" && errors.Is(cause, modeltelemetry.ErrToolCallFormat) {
		return &modelInvocationError{cause: &orchestration.ExecutionStop{
			Kind:  orchestration.ExecutionToolCallFormat,
			Cause: fmt.Errorf("%w (request %s)", cause, requestID),
		}}
	}
	return &modelInvocationError{cause: orchestration.FromModelExecutionError(cause)}
}

func (m *recordingModel) settleUsage(ctx context.Context, requestID string, message *schema.Message) error {
	estimate, exists := m.estimates.Load(requestID)
	if !exists {
		return nil
	}
	usage := estimate.(tokenusage.Snapshot)
	// Input is a tokenizer estimate; output retains the requested allowance.
	// This is a conservative reservation, not a proven total-token upper bound.
	quality := "estimated_reservation"
	if message != nil && message.ResponseMeta != nil && message.ResponseMeta.Usage != nil {
		usage = tokenusage.FromMessage(message)
		quality = "provider"
	}
	if m.factory != nil && m.factory.config.UsageLedger != nil {
		config := m.factory.config
		cost := int64(0)
		if config.Pricing != nil {
			if calculated, ok := config.Pricing.CostMicros(config.Provider, config.Model, usage); ok {
				cost = calculated
			} else {
				quality += "_unpriced"
			}
		}
		record := tokenusage.Record{TeamID: config.ActorScope.TeamID, AgentID: config.ActorScope.AgentID, RunID: config.ActorScope.RunID, TurnID: m.turnID, RequestID: requestID, ModelProfile: config.ModelProfile, Provider: config.Provider, Model: config.Model, Usage: usage, CostMicros: cost, Quality: quality}
		durable, cancel := durabilityContext(ctx)
		defer cancel()
		var err error
		if id, ok := m.reservations.Load(requestID); ok {
			if durableLedger, ok := config.UsageLedger.(tokenusage.DurableReservationLedger); ok {
				if err := durableLedger.StageSettlement(durable, id.(string), record); err != nil {
					return fmt.Errorf("persist model settlement receipt: %w", err)
				}
			}
			if ledger, ok := config.UsageLedger.(tokenusage.ReservationLedger); ok {
				err = ledger.SettleReservation(durable, id.(string), record)
			} else {
				err = config.UsageLedger.RecordUsage(durable, record)
			}
		} else {
			err = config.UsageLedger.RecordUsage(durable, record)
		}
		if err != nil {
			return fmt.Errorf("settle model usage: %w", err)
		}
	}
	m.reservations.Delete(requestID)
	m.estimates.Delete(requestID)
	return nil
}

func (m *recordingModel) markDispatched(ctx context.Context, requestID string) error {
	if m.factory == nil {
		return nil
	}
	ledger, ok := m.factory.config.UsageLedger.(tokenusage.DurableReservationLedger)
	if !ok {
		return nil
	}
	id, ok := m.reservations.Load(requestID)
	if !ok {
		return errors.New("durable model request has no reservation")
	}
	m.emitLive(ctx, m.modelLifecycleEvent(EventModelDispatchPersistenceStarted))
	if err := ledger.MarkDispatched(ctx, id.(string)); err != nil {
		return fmt.Errorf("persist model dispatch boundary: %w", err)
	}
	return nil
}

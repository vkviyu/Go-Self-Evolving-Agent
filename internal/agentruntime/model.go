package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/experienceport"
	"github.com/vkviyu/go-self-evolving-agent/internal/modeltelemetry"
	"github.com/vkviyu/go-self-evolving-agent/internal/observation"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

type recordingModel struct {
	inner             model.BaseChatModel
	factory           *Factory
	events            session.EventStore
	sessionID         session.ID
	turnID            string
	runtimeSnapshotID string
	requestSequence   atomic.Uint64
	reservations      sync.Map
	estimates         sync.Map
	purpose           string
	contextState      requestContextState
	assessment        requestAssessment
	background        backgroundState
	replay            *journalReplay
	receipt           modelReceipt
	toolSnapshot      *tool.Snapshot
	openingInput      string
	experienceContext *experienceport.Context
	experienceRetry   *experienceJob
	budgetSessionID   session.ID
	liveSink          EventSink
	correctToolFormat bool
}

var _ model.BaseChatModel = (*recordingModel)(nil)

func (m *recordingModel) Generate(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.Message, error) {
	m.receipt = modelReceipt{Version: 2, RawDigest: rawRequestDigest(input)}
	if response, replayed, err := m.replayResponse(ctx, input, options); replayed {
		return response, err
	}
	input, options, corrected, err := m.prepareDispatchInput(ctx, input, options)
	if err != nil {
		return nil, err
	}
	if m.factory != nil && m.factory.tokenizer != nil {
		m.emitLive(ctx, m.modelLifecycleEvent(EventContextTokenCountingStarted))
	}
	estimated := m.countRequestInput(input, options)
	requestID := m.requestID()
	ctx = m.observeRequestContext(ctx, requestID)
	if err := m.reserve(ctx, estimated, requestID, options); err != nil {
		return nil, fmt.Errorf("reserve model budget: %w", err)
	}
	if err := m.recordModelRequest(ctx, input, options, false, corrected, estimated, requestID); err != nil {
		m.releaseUnsent(ctx, requestID, err.Error())
		return nil, fmt.Errorf("record model request: %w", err)
	}
	if err := m.checkExperience(ctx); err != nil {
		m.releaseUnsent(ctx, requestID, err.Error())
		return nil, err
	}
	if err := m.markDispatched(ctx, requestID); err != nil {
		m.releaseUnsent(ctx, requestID, err.Error())
		return nil, err
	}
	m.emitLive(ctx, m.modelLifecycleEvent(EventModelRequestStarted))
	response, err := m.inner.Generate(ctx, input, options...)
	if err != nil {
		return nil, m.failRequest(ctx, requestID, err, false)
	}
	if response == nil {
		return nil, m.failRequest(ctx, requestID, errors.New("model returned a nil message"), false)
	}
	m.emitLive(ctx, m.modelLifecycleEvent(EventModelResponseStarted))
	if err := m.recordAssistantMessage(ctx, response, false, requestID); err != nil {
		return nil, fmt.Errorf("record assistant message: %w", err)
	}
	m.observeProviderUsage(response, estimated, requestID)
	return response, nil
}

func (m *recordingModel) Stream(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	m.receipt = modelReceipt{Version: 2, RawDigest: rawRequestDigest(input)}
	if response, replayed, err := m.replayResponse(ctx, input, options); replayed {
		if err != nil {
			return nil, err
		}
		return schema.StreamReaderFromArray([]*schema.Message{response}), nil
	}
	input, options, corrected, err := m.prepareDispatchInput(ctx, input, options)
	if err != nil {
		return nil, err
	}
	if m.factory != nil && m.factory.tokenizer != nil {
		m.emitLive(ctx, m.modelLifecycleEvent(EventContextTokenCountingStarted))
	}
	estimated := m.countRequestInput(input, options)
	requestID := m.requestID()
	ctx = m.observeRequestContext(ctx, requestID)
	if err := m.reserve(ctx, estimated, requestID, options); err != nil {
		return nil, fmt.Errorf("reserve model budget: %w", err)
	}
	if err := m.recordModelRequest(ctx, input, options, true, corrected, estimated, requestID); err != nil {
		m.releaseUnsent(ctx, requestID, err.Error())
		return nil, fmt.Errorf("record streaming model request: %w", err)
	}
	if err := m.checkExperience(ctx); err != nil {
		m.releaseUnsent(ctx, requestID, err.Error())
		return nil, err
	}
	if err := m.markDispatched(ctx, requestID); err != nil {
		m.releaseUnsent(ctx, requestID, err.Error())
		return nil, err
	}
	m.emitLive(ctx, m.modelLifecycleEvent(EventModelRequestStarted))
	ctx = modeltelemetry.CaptureStream(ctx)
	upstream, err := m.inner.Stream(ctx, input, options...)
	if err != nil {
		return failedMessageStream(m.failRequest(ctx, requestID, err, true)), nil
	}
	if upstream == nil {
		streamErr := errors.New("model returned a nil stream")
		return failedMessageStream(m.failRequest(ctx, requestID, streamErr, true)), nil
	}
	m.emitLive(ctx, m.modelLifecycleEvent(EventModelResponseStarted))

	reader, writer := schema.Pipe[*schema.Message](16)
	go func() {
		defer writer.Close()
		defer upstream.Close()
		chunks := make([]*schema.Message, 0, 8)
		observeOutput := observation.Enabled(ctx)
		for {
			chunk, recvErr := upstream.Recv()
			if errors.Is(recvErr, io.EOF) {
				break
			}
			if recvErr != nil {
				writer.Send(nil, m.failRequest(ctx, requestID, recvErr, true))
				return
			}
			if chunk == nil {
				err := errors.New("model stream returned a nil message")
				writer.Send(nil, m.failRequest(ctx, requestID, err, true))
				return
			}
			stableChunk, cloneErr := cloneMessage(chunk)
			if cloneErr != nil {
				writer.Send(nil, m.failRequest(ctx, requestID, cloneErr, true))
				return
			}
			chunks = append(chunks, stableChunk)
			// Persist before publishing a delta. Partial output is transcript
			// evidence only; it is never replayed as a completed model decision.
			if m.purpose == "" {
				durable, cancel := durabilityContext(ctx)
				_, appendErr := m.events.Append(durable, m.sessionID, session.AnySequence, session.PendingEvent{
					Type: session.EventModelPartial, CorrelationID: m.turnID, RuntimeSnapshotID: m.runtimeSnapshotID,
					Payload: session.MustJSONPayload(map[string]any{"request_id": requestID, "index": len(chunks), "chunk": stableChunk}),
				})
				cancel()
				if appendErr != nil {
					writer.Send(nil, m.failRequest(ctx, requestID, fmt.Errorf("record partial model output: %w", appendErr), true))
					return
				}
			}
			// Eino annotates messages with IDs while consuming the stream. Keep
			// the copy used for final concatenation private from that mutation.
			outgoing, cloneErr := cloneMessage(stableChunk)
			if cloneErr != nil {
				writer.Send(nil, m.failRequest(ctx, requestID, cloneErr, true))
				return
			}
			if observeOutput && (outgoing.Content != "" || outgoing.ReasoningContent != "" || len(outgoing.ToolCalls) > 0) {
				observeOutput = false
				observation.Emit(ctx, observation.Event{Component: "runtime", Name: "model.first_output.forwarding"})
			}
			if writer.Send(outgoing, nil) {
				_ = m.failRequest(ctx, requestID, errors.New("stream consumer closed"), true)
				return
			}
		}
		message, concatErr := schema.ConcatMessages(chunks)
		if concatErr != nil {
			writer.Send(nil, m.failRequest(ctx, requestID, concatErr, true))
			return
		}
		if message == nil {
			err := errors.New("model stream completed without a message")
			writer.Send(nil, m.failRequest(ctx, requestID, err, true))
			return
		}
		if appendErr := m.recordAssistantMessage(ctx, message, true, requestID); appendErr != nil {
			writer.Send(nil, fmt.Errorf("record streamed assistant message: %w", appendErr))
			return
		}
		m.observeProviderUsage(message, estimated, requestID)
	}()
	return reader, nil
}

// emitLive delivers presentation-only state through the request context. The
// lifecycle boundary must not be dropped: otherwise the TUI can remain in
// "Preparing request" after the provider call has already started. A closed
// or canceled consumer still releases the provider through ctx.
func (m *recordingModel) emitLive(ctx context.Context, event Event) {
	if m == nil {
		return
	}
	_ = emitStage(ctx, m.liveSink, event)
}

func (m *recordingModel) modelLifecycleEvent(eventType EventType) Event {
	return Event{Type: eventType, TurnID: m.turnID, Purpose: m.purpose}
}

func cloneMessage(message *schema.Message) (*schema.Message, error) {
	encoded, err := json.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("clone streamed message: %w", err)
	}
	var clone schema.Message
	if err := json.Unmarshal(encoded, &clone); err != nil {
		return nil, fmt.Errorf("decode cloned streamed message: %w", err)
	}
	return &clone, nil
}

func failedMessageStream(cause error) *schema.StreamReader[*schema.Message] {
	reader, writer := schema.Pipe[*schema.Message](1)
	go func() {
		defer writer.Close()
		writer.Send(nil, cause)
	}()
	return reader
}

func (m *recordingModel) requestID() string {
	return fmt.Sprintf("%s:%d", m.turnID, m.requestSequence.Add(1))
}

func (m *recordingModel) observeProviderUsage(message *schema.Message, estimated int, requestID string) {
	if m.factory == nil || message == nil || message.ResponseMeta == nil || message.ResponseMeta.Usage == nil {
		return
	}
	actual := message.ResponseMeta.Usage.PromptTokens
	if actual > 0 {
		m.factory.observeProviderInput(requestID, actual, estimated)
	}
}

func (m *recordingModel) recordModelRequest(ctx context.Context, input []*schema.Message, options []model.Option, streaming, corrected bool, estimated int, requestID string) error {
	common := model.GetCommonOptions(nil, options...)
	payload := map[string]any{
		"turn_id":                m.turnID,
		"message_count":          len(input),
		"tool_count":             len(common.Tools),
		"estimated_input_tokens": estimated,
		"request_id":             requestID,
		"request_sequence":       m.requestSequence.Load(),
		"purpose":                m.purpose,
	}
	if m.factory != nil && !m.factory.config.Execution.IsZero() {
		payload["execution"] = m.factory.config.Execution
	}
	if common.MaxTokens != nil {
		payload["output_token_limit"] = *common.MaxTokens
	}
	if streaming {
		payload["stream"] = true
	}
	if corrected {
		payload["tool_format_corrected"] = true
	}
	if m.purpose == "experience" {
		// Retain the original auxiliary evidence, not only its token estimate.
		payload["messages"] = input
		payload["budget_session_id"] = m.usageSessionID()
		payload["actor_scope"] = m.factory.config.ActorScope
	}
	m.emitLive(ctx, m.modelLifecycleEvent(EventModelRequestPersistenceStarted))
	// Persist the source sequence before mirroring usage. A failed mirror must
	// never allow the next retry to reuse this request identity.
	_, err := m.events.Append(ctx, m.sessionID, session.AnySequence, session.PendingEvent{
		Type:              session.EventModelRequested,
		CorrelationID:     m.turnID,
		RuntimeSnapshotID: m.runtimeSnapshotID,
		Payload:           session.MustJSONPayload(payload),
	})
	if err != nil {
		return err
	}
	return m.recordHostMaintenanceUsage(ctx, session.EventModelRequested, payload)
}

func (m *recordingModel) recordAssistantMessage(ctx context.Context, message *schema.Message, streaming bool, requestID string) (err error) {
	defer func() { observeModelFinished(ctx, message, err) }()
	// The provider response is already complete when this method runs. Its
	// transcript event and usage settlement must survive a turn deadline or
	// cancellation so the caller can classify the finished turn correctly.
	durable, cancel := durabilityContext(ctx)
	defer cancel()
	payload := map[string]any{
		"turn_id":    m.turnID,
		"content":    message.Content,
		"tool_calls": message.ToolCalls,
		"request_id": requestID,
		"purpose":    m.purpose,
	}
	if m.factory != nil && !m.factory.config.Execution.IsZero() {
		payload["execution"] = m.factory.config.Execution
	}
	if m.purpose == "experience" {
		payload["message"] = message
	}
	if m.purpose == "" && m.receipt.RawDigest != "" {
		m.receipt.Response = message
		payload["receipt"] = m.receipt
	}
	if err := m.settleUsage(durable, requestID, message); err != nil {
		return err
	}
	if message.ResponseMeta == nil || message.ResponseMeta.Usage == nil {
		payload["usage_quality"] = "estimated_reservation"
	} else {
		payload["usage_quality"] = "provider"
	}
	if message.ResponseMeta != nil && message.ResponseMeta.FinishReason != "" {
		// Preserve the provider's terminal reason separately from token usage.
		// This distinguishes an output cap from a completed answer when
		// investigating a transcript, including responses without usage.
		payload["finish_reason"] = message.ResponseMeta.FinishReason
	}
	if message.ResponseMeta != nil && message.ResponseMeta.Usage != nil {
		payload["prompt_tokens"] = message.ResponseMeta.Usage.PromptTokens
		payload["cached_prompt_tokens"] = message.ResponseMeta.Usage.PromptTokenDetails.CachedTokens
		payload["completion_tokens"] = message.ResponseMeta.Usage.CompletionTokens
		payload["reasoning_tokens"] = message.ResponseMeta.Usage.CompletionTokensDetails.ReasoningTokens
		payload["total_tokens"] = message.ResponseMeta.Usage.TotalTokens
		payload["request_id"] = requestID
	}
	if streaming {
		payload["stream"] = true
		if observation := modeltelemetry.ReadStream(ctx); observation != nil {
			payload["transport"] = observation
		}
	}
	if m.purpose == "experience" {
		payload["budget_session_id"] = m.usageSessionID()
		payload["actor_scope"] = m.factory.config.ActorScope
	}
	_, err = m.events.Append(durable, m.sessionID, session.AnySequence, session.PendingEvent{
		Type:              session.EventAssistantMessage,
		CorrelationID:     m.turnID,
		RuntimeSnapshotID: m.runtimeSnapshotID,
		Payload:           session.MustJSONPayload(payload),
	})
	if err != nil {
		return err
	}
	return m.recordHostMaintenanceUsage(durable, session.EventAssistantMessage, payload)
}

func (m *recordingModel) reserve(ctx context.Context, estimated int, requestID string, options []model.Option) error {
	m.emitLive(ctx, m.modelLifecycleEvent(EventModelBudgetReservationStarted))
	output := 0
	common := model.GetCommonOptions(nil, options...)
	if common.MaxTokens != nil {
		output = *common.MaxTokens
	}
	var contract orchestration.ExecutionContract
	if m.factory != nil {
		contract = m.factory.config.ExecutionContract
	}
	if contract.MaxOutputTokens > 0 && (output == 0 || contract.MaxOutputTokens < output) {
		output = contract.MaxOutputTokens
	}
	if contract.MaxInputTokens > 0 && estimated > contract.MaxInputTokens {
		return &orchestration.ExecutionStop{Kind: orchestration.ExecutionBudget, Cause: fmt.Errorf("%w: execution input allowance exceeded", tokenusage.ErrBudgetExceeded)}
	}
	if contract.MaxTotalTokens > 0 && estimated+output > contract.MaxTotalTokens {
		return &orchestration.ExecutionStop{Kind: orchestration.ExecutionBudget, Cause: fmt.Errorf("%w: execution total allowance exceeded", tokenusage.ErrBudgetExceeded)}
	}
	estimate := tokenusage.Snapshot{Input: estimated, Output: output, Total: estimated + output}
	m.estimates.Store(requestID, estimate)
	if m.factory == nil || m.factory.config.UsageLedger == nil {
		return nil
	}
	ledger, ok := m.factory.config.UsageLedger.(tokenusage.ReservationLedger)
	if !ok {
		return nil
	}
	cost := int64(0)
	if pricing := m.factory.config.Pricing; pricing != nil {
		if reservationPricing, ok := pricing.(tokenusage.ReservationPricer); ok {
			cost, _ = reservationPricing.ReservationCostMicros(m.factory.config.Provider, m.factory.config.Model, estimate)
		} else {
			cost, _ = pricing.CostMicros(m.factory.config.Provider, m.factory.config.Model, estimate)
		}
	}
	if contract.MaxCostMicros > 0 && cost > contract.MaxCostMicros {
		m.estimates.Delete(requestID)
		return &orchestration.ExecutionStop{Kind: orchestration.ExecutionBudget, Cause: fmt.Errorf("%w: execution cost allowance exceeded", tokenusage.ErrBudgetExceeded)}
	}
	reservation, err := ledger.Reserve(ctx, tokenusage.ReservationRequest{
		TeamID: m.factory.config.ActorScope.TeamID, AgentID: m.factory.config.ActorScope.AgentID,
		RunID: m.factory.config.ActorScope.RunID, TurnID: m.turnID, RequestID: requestID,
		ContractHash: m.factory.config.Execution.ContractHash,
		ExecutionBudget: tokenusage.BudgetLimits{
			MaxInputTokens: contract.MaxInputTokens, MaxOutputTokens: contract.MaxOutputTokens,
			MaxTotalTokens: contract.MaxTotalTokens, MaxCostMicros: contract.MaxCostMicros,
		},
		InputTokens: estimated, OutputTokens: output, TotalTokens: estimated + output, CostMicros: cost,
		ModelProfile: m.factory.config.ModelProfile, Provider: m.factory.config.Provider, Model: m.factory.config.Model,
	})
	if err != nil {
		m.estimates.Delete(requestID)
		if errors.Is(err, tokenusage.ErrBudgetExceeded) {
			return &orchestration.ExecutionStop{Kind: orchestration.ExecutionBudget, Cause: err}
		}
		return err
	}
	if reservation.Dispatched || (reservation.State != "" && reservation.State != tokenusage.ReservationPending) {
		m.estimates.Delete(requestID)
		return errors.New("model request identity was already dispatched or closed; refusing reuse")
	}
	m.reservations.Store(requestID, reservation.ID)
	return nil
}

func (m *recordingModel) releaseUnsent(ctx context.Context, requestID, reason string) {
	m.estimates.Delete(requestID)
	if m.factory == nil || m.factory.config.UsageLedger == nil {
		return
	}
	reservationID, ok := m.reservations.LoadAndDelete(requestID)
	if !ok {
		return
	}
	if ledger, supportsReservations := m.factory.config.UsageLedger.(tokenusage.ReservationLedger); supportsReservations {
		durable, cancel := durabilityContext(ctx)
		defer cancel()
		_ = ledger.ReleaseReservation(durable, reservationID.(string), reason)
	}
}

func (m *recordingModel) recordModelFailure(ctx context.Context, requestID string, cause error, streaming bool) error {
	recordContext, cancel := durabilityContext(ctx)
	defer cancel()
	payload := map[string]any{"turn_id": m.turnID, "request_id": requestID, "purpose": m.purpose, "error": cause.Error()}
	if streaming {
		payload["stream"] = true
		if observation := modeltelemetry.ReadStream(ctx); observation != nil {
			payload["transport"] = observation
		}
	}
	_, err := m.events.Append(recordContext, m.sessionID, session.AnySequence, session.PendingEvent{
		Type:              session.EventModelFailed,
		CorrelationID:     m.turnID,
		RuntimeSnapshotID: m.runtimeSnapshotID,
		Payload:           session.MustJSONPayload(payload),
	})
	return err
}

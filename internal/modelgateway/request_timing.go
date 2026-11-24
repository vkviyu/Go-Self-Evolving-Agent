package modelgateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// requestTimingModel applies the gateway's model timeout to both Eino adapter
// entry points. Stream calls mark the request for the HTTP transport because
// their lifetime continues after Stream returns.
type requestTimingModel struct {
	model.ToolCallingChatModel
	timeout            time.Duration
	preserveReadErrors bool
}

type modelRequestKey struct{}

type modelRequestState struct {
	streaming bool
	readError atomic.Pointer[modelReadError]
	// The native Responses decoder and its Body.Read calls run on the same
	// goroutine. Other adapters leave this unset.
	events *modelEventWatchdog
}

type modelReadError struct{ error }

func (m *requestTimingModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	bound, err := m.ToolCallingChatModel.WithTools(tools)
	if err != nil {
		return nil, err
	}
	copyModel := *m
	copyModel.ToolCallingChatModel = bound
	return &copyModel, nil
}

func (m *requestTimingModel) Generate(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.Message, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, m.timeout, &modelDeadline{phase: "request", timeout: m.timeout})
	defer cancel()
	var state *modelRequestState
	if m.preserveReadErrors {
		state = new(modelRequestState)
		ctx = context.WithValue(ctx, modelRequestKey{}, state)
	}
	message, err := m.ToolCallingChatModel.Generate(ctx, input, options...)
	if err == nil && state != nil {
		if readErr := state.readError.Load(); readErr != nil {
			return nil, readErr.error
		}
	}
	if cause, ok := context.Cause(ctx).(*modelDeadline); ok {
		return nil, cause
	}
	return message, err
}

func (m *requestTimingModel) Stream(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	state := &modelRequestState{streaming: true}
	upstream, err := m.ToolCallingChatModel.Stream(context.WithValue(ctx, modelRequestKey{}, state), input, options...)
	if err != nil || upstream == nil || !m.preserveReadErrors {
		return upstream, err
	}

	// The Ollama adapter's HTTP client can return clean stream EOF after a
	// body read failure. Preserve that failure so a partial reply cannot be
	// accepted by the Agent loop as a confirmed model decision.
	reader, writer := schema.Pipe[*schema.Message](1)
	go func() {
		defer writer.Close()
		defer upstream.Close()
		for {
			message, err := upstream.Recv()
			if errors.Is(err, io.EOF) {
				if readErr := state.readError.Load(); readErr != nil {
					writer.Send(nil, readErr.error)
				}
				return
			}
			if writer.Send(message, err) || err != nil {
				return
			}
		}
	}()
	return reader, nil
}

type modelDeadline struct {
	phase   string
	timeout time.Duration
}

func (err *modelDeadline) Error() string {
	return fmt.Sprintf("model %s timed out after %s", err.phase, err.timeout)
}

func (*modelDeadline) Unwrap() error { return context.DeadlineExceeded }

func modelTimingHTTPClient(client *http.Client, headers, idle time.Duration) *http.Client {
	clone := &http.Client{}
	if client != nil {
		*clone = *client
	}
	inner := clone.Transport
	if inner == nil {
		inner = http.DefaultTransport
	}
	// Honor an explicitly supplied Client.Timeout as an independent hard cap.
	clone.Transport = modelTimingTransport{inner: inner, headers: headers, idle: idle}
	return clone
}

type modelTimingTransport struct {
	inner   http.RoundTripper
	headers time.Duration
	idle    time.Duration
}

func (transport modelTimingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	ctx := request.Context()
	state, _ := ctx.Value(modelRequestKey{}).(*modelRequestState)
	if state == nil {
		return transport.inner.RoundTrip(request)
	}
	var cancel context.CancelCauseFunc
	var stopHeaders func()
	if state.streaming {
		ctx, cancel = context.WithCancelCause(ctx)
		stopHeaders = armModelDeadline(transport.headers, &modelDeadline{phase: "response headers", timeout: transport.headers}, cancel)
	}
	if state.streaming || request.Cancel != nil {
		request = request.WithContext(ctx)
		// The model adapters use context cancellation. A timed http.Client
		// additionally attaches a legacy Cancel channel when the transport is
		// wrapped. Use its context deadline so a captured Ollama body error
		// consistently retains DeadlineExceeded instead of a legacy cancel.
		request.Cancel = nil
	}
	response, err := transport.inner.RoundTrip(request)
	if stopHeaders != nil {
		stopHeaders()
	}
	if cause, ok := context.Cause(ctx).(*modelDeadline); ok {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		return nil, cause
	}
	if err != nil || response == nil || response.Body == nil {
		if cancel != nil {
			cancel(nil)
		}
		return response, err
	}
	response.Body = &modelTimingBody{
		ReadCloser: response.Body, ctx: ctx, cancel: cancel, state: state,
		idle: &modelDeadline{phase: "stream read", timeout: transport.idle},
	}
	return response, nil
}

type modelTimingBody struct {
	io.ReadCloser
	ctx    context.Context
	cancel context.CancelCauseFunc
	state  *modelRequestState
	idle   *modelDeadline
}

func (body *modelTimingBody) Read(p []byte) (int, error) {
	var stopRead func()
	if body.cancel != nil && len(p) != 0 {
		delay, failure := body.idle.timeout, body.idle
		if events := body.state.events; events != nil && events.sawBytes {
			if remaining := time.Until(events.deadline); remaining < delay {
				delay, failure = remaining, &events.failure
			}
		}
		if delay <= 0 {
			body.cancel(failure)
			err := context.Cause(body.ctx)
			body.state.readError.Store(&modelReadError{err})
			return 0, err
		}
		stopRead = armModelDeadline(delay, failure, body.cancel)
	}
	n, err := body.ReadCloser.Read(p)
	if stopRead != nil {
		stopRead()
	}
	if n > 0 && body.state.events != nil {
		body.state.events.sawBytes = true
	}
	if cause, ok := context.Cause(body.ctx).(*modelDeadline); ok {
		err = cause
	}
	if err != nil && !errors.Is(err, io.EOF) {
		body.state.readError.Store(&modelReadError{err})
	}
	return n, err
}

func armModelDeadline(delay time.Duration, failure *modelDeadline, cancel context.CancelCauseFunc) func() {
	done := make(chan struct{})
	timer := time.AfterFunc(delay, func() {
		cancel(failure)
		close(done)
	})
	return func() {
		// If the callback won the race at the deadline, let it set the cause
		// before the caller decides whether this read succeeded.
		if !timer.Stop() {
			<-done
		}
	}
}

func (body *modelTimingBody) Close() error {
	if body.cancel != nil {
		defer body.cancel(nil)
	}
	return body.ReadCloser.Close()
}

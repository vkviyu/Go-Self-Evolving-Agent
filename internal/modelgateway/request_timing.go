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

func modelTimingHTTPClient(client *http.Client, timeout time.Duration) *http.Client {
	clone := &http.Client{}
	if client != nil {
		*clone = *client
	}
	inner := clone.Transport
	if inner == nil {
		inner = http.DefaultTransport
	}
	// Honor an explicitly supplied Client.Timeout as an independent hard cap.
	clone.Transport = modelTimingTransport{inner: inner, timeout: timeout}
	return clone
}

type modelTimingTransport struct {
	inner   http.RoundTripper
	timeout time.Duration
}

func (transport modelTimingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	ctx := request.Context()
	state, _ := ctx.Value(modelRequestKey{}).(*modelRequestState)
	if state == nil {
		return transport.inner.RoundTrip(request)
	}
	var cancel context.CancelCauseFunc
	var headersTimer *time.Timer
	if state.streaming {
		ctx, cancel = context.WithCancelCause(ctx)
		headersTimer = time.AfterFunc(transport.timeout, func() {
			cancel(&modelDeadline{phase: "response headers", timeout: transport.timeout})
		})
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
	if headersTimer != nil {
		headersTimer.Stop()
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
		idle: &modelDeadline{phase: "stream read", timeout: transport.timeout},
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
	var timer *time.Timer
	if body.cancel != nil && len(p) != 0 {
		// Bound time blocked on a transport read. Bytes such as SSE keepalive
		// frames count as transport activity, even if no token is emitted.
		timer = time.AfterFunc(body.idle.timeout, func() { body.cancel(body.idle) })
	}
	n, err := body.ReadCloser.Read(p)
	if timer != nil {
		timer.Stop()
	}
	if cause, ok := context.Cause(body.ctx).(*modelDeadline); ok {
		err = cause
	}
	if err != nil && !errors.Is(err, io.EOF) {
		body.state.readError.Store(&modelReadError{err})
	}
	return n, err
}

func (body *modelTimingBody) Close() error {
	if body.cancel != nil {
		defer body.cancel(nil)
	}
	return body.ReadCloser.Close()
}

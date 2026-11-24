package modelgateway

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/modeltelemetry"
)

type observedResponsesBody struct {
	io.ReadCloser
	observation *responsesObservation
}

func (body observedResponsesBody) Read(p []byte) (int, error) {
	n, err := body.ReadCloser.Read(p)
	if n > 0 {
		now := time.Now()
		if body.observation.FirstByteAt.IsZero() {
			body.observation.FirstByteAt = now.UTC()
			if s := body.observation.stats; s != nil {
				s.emit("responses.first_byte", "received", nil, nil)
			}
		}
		body.observation.LastByteAt = now.UTC()
		body.observation.BodyBytes += uint64(n)
		if body.observation.stats != nil {
			body.observation.activity(now)
		}
	}
	return n, err
}

func observeResponsesHeaders(response *http.Response, observation *responsesObservation, apiKey string) {
	observation.HeadersAt = time.Now().UTC()
	observation.Status = response.StatusCode
	rawType := response.Header.Get("Content-Type")
	if rawType != "" {
		mediaType, _, _ := mime.ParseMediaType(rawType)
		switch mediaType {
		case "text/event-stream", "application/json", "text/html", "text/plain", "application/octet-stream":
			observation.ContentType = mediaType
		default:
			observation.ContentType = "other"
		}
	}
	for _, key := range []string{"X-Request-Id", "OpenAI-Request-Id", "CF-Ray"} {
		if value := response.Header.Get(key); value != apiKey && validProviderRequestID(value) {
			observation.RequestID = value
			break
		}
	}
	observation.headers()
	response.Body = observedResponsesBody{ReadCloser: response.Body, observation: observation}
}

func validProviderRequestID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for i := range len(value) {
		b := value[i]
		if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '-' || b == '_' || b == '.') {
			return false
		}
	}
	return true
}

func responsesEventLabel(name string) string {
	switch name {
	case "response.created", "response.queued", "response.in_progress",
		"response.completed", "response.incomplete", "response.failed", "response.cancelled", "error",
		"response.output_item.added", "response.output_item.done",
		"response.content_part.added", "response.content_part.done",
		"response.output_text.delta", "response.output_text.done",
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done",
		"response.reasoning_summary_text.delta", "response.reasoning_summary_text.done",
		"response.function_call_arguments.delta", "response.function_call_arguments.done",
		"response.custom_tool_call_input.delta", "response.custom_tool_call_input.done":
		return name
	default:
		return "other"
	}
}

func recordResponsesTransport(ctx context.Context, observation *responsesObservation, err error) {
	var deadline *modelDeadline
	var status *HTTPStatusError
	switch {
	case observation.Completed:
		observation.Outcome = "completed"
	case errors.As(err, &deadline) && deadline.phase == "response headers":
		observation.Outcome = "response_headers_timeout"
	case errors.As(err, &deadline) && deadline.phase == "stream read":
		observation.Outcome = "stream_read_timeout"
	case errors.As(err, &deadline) && deadline.phase == "stream event idle":
		observation.Outcome = "stream_event_idle_timeout"
	case errors.As(err, &status):
		observation.Outcome = "http_status_error"
	case errors.Is(err, context.Canceled):
		observation.Outcome = "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		observation.Outcome = "deadline_exceeded"
	case errors.Is(err, ErrResponsesStreamTruncated):
		observation.Outcome = "truncated"
	case errors.Is(err, modeltelemetry.ErrToolCallFormat):
		observation.Outcome = "tool_call_format_rejected"
	case err == nil:
		observation.Outcome = "stream_closed"
	case observation.HeadersAt.IsZero():
		observation.Outcome = "request_failed"
	default:
		observation.Outcome = "stream_failed"
	}
	observation.EndedAt = time.Now().UTC()
	modeltelemetry.RecordStream(ctx, observation.Stream)
	observation.finish()
}

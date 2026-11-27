// Package modeltelemetry carries per-call diagnostics from a model adapter to
// the runtime that records the call's outcome.
package modeltelemetry

import (
	"context"
	"sync/atomic"
	"time"
)

type Stream struct {
	StartedAt   time.Time `json:"started_at"`
	HeadersAt   time.Time `json:"headers_at,omitzero"`
	FirstByteAt time.Time `json:"first_byte_at,omitzero"`
	LastByteAt  time.Time `json:"last_byte_at,omitzero"`
	EndedAt     time.Time `json:"ended_at"`
	BodyBytes   uint64    `json:"body_bytes"`
	Status      int       `json:"status,omitempty"`
	ContentType string    `json:"content_type,omitempty"`
	RequestID   string    `json:"request_id,omitempty"`
	LastEvent   string    `json:"last_event,omitempty"`
	Completed   bool      `json:"completed"`
	Outcome     string    `json:"outcome"`
}

type streamKey struct{}

func CaptureStream(ctx context.Context) context.Context {
	return context.WithValue(ctx, streamKey{}, new(atomic.Pointer[Stream]))
}

func RecordStream(ctx context.Context, stream Stream) {
	if capture, ok := ctx.Value(streamKey{}).(*atomic.Pointer[Stream]); ok {
		capture.CompareAndSwap(nil, &stream)
	}
}

// ReadStream returns the most recently captured stream telemetry, or nil if none has been
// published.
func ReadStream(ctx context.Context) *Stream {
	if capture, ok := ctx.Value(streamKey{}).(*atomic.Pointer[Stream]); ok {
		return capture.Load()
	}
	return nil
}

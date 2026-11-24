package modelgateway

import (
	"context"
	"time"
)

// modelEventWatchdog bounds metadata-only activity on a live SSE stream.
// It runs only while the parser is reading, so backpressure from the local
// consumer cannot consume the provider's idle allowance.
type modelEventWatchdog struct {
	failure   modelDeadline
	deadline  time.Time
	sawBytes  bool
	lifecycle map[string]bool
}

func startModelEventWatchdog(ctx context.Context, timeout time.Duration) *modelEventWatchdog {
	state, _ := ctx.Value(modelRequestKey{}).(*modelRequestState)
	if state == nil || timeout <= 0 {
		return nil
	}
	w := &modelEventWatchdog{
		failure:   modelDeadline{phase: "stream event idle", timeout: timeout},
		deadline:  time.Now().Add(timeout),
		lifecycle: make(map[string]bool, 3),
	}
	state.events = w
	return w
}

func (w *modelEventWatchdog) note(event responsesStreamEvent) {
	if w == nil {
		return
	}
	switch event.Type {
	case "response.created", "response.queued", "response.in_progress":
		if w.lifecycle[event.Type] || len(event.Response) == 0 {
			return
		}
		w.lifecycle[event.Type] = true
	case "response.output_text.delta", "response.reasoning_summary_text.delta",
		"response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		if event.Delta == "" {
			return
		}
	case "response.output_item.added", "response.output_item.done":
		// These frames have already passed item decoding.
	case "response.content_part.added", "response.content_part.done",
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		if event.ItemID == "" {
			return
		}
	default:
		return
	}
	// Called after message delivery: include only time until the next read.
	w.deadline = time.Now().Add(w.failure.timeout)
	w.sawBytes = false
}

package ateprocess

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/observation"
)

const (
	StageQueue      Stage = "queue"
	StageOwnership  Stage = "ownership"
	StageInitialize Stage = "initialize"
	StageStart      Stage = "start"
	StageHandshake  Stage = "handshake"
	StageOperation  Stage = "operation"
	StageShutdown   Stage = "shutdown"
	StageDone       Stage = "done"
)

type (
	// Stage identifies a local storage wait, independently of the caller's task.
	Stage string

	// Progress contains timing metadata only, never paths, credentials or payloads.
	Progress struct {
		BatchID uint64
		Stage   Stage
		At      time.Time
	}

	// Observer must return promptly and support concurrent batches. Observation
	// cannot change storage outcomes or cause a callback to be retried.
	Observer func(context.Context, Progress)

	observerKey struct{}
)

var nextBatchID atomic.Uint64

// WithObserver observes only batches reached through this context.
func WithObserver(ctx context.Context, observer Observer) context.Context {
	return context.WithValue(ctx, observerKey{}, observer)
}

// batchProgress brackets a batch including queuing and deferred shutdown.
func batchProgress(ctx context.Context) func(Stage) {
	observer, _ := ctx.Value(observerKey{}).(Observer)
	if observer == nil && !observation.Enabled(ctx) {
		return func(Stage) {}
	}
	id := nextBatchID.Add(1)
	return func(stage Stage) {
		if observer != nil {
			observer(ctx, Progress{BatchID: id, Stage: stage, At: time.Now()})
		}
		if observation.Enabled(ctx) {
			observation.Emit(ctx, observation.Event{Component: "ate", Name: "connection.stage", Status: string(stage), Metrics: map[string]int64{"batch": int64(id)}})
		}
	}
}

func observeLease(ctx context.Context) (acquired func(), finish func(error)) {
	id := int64(nextBatchID.Add(1))
	start := time.Now()
	var operation time.Time
	emit := func(name, status string) {
		metrics := map[string]int64{"batch": id, "elapsed_us": time.Since(start).Microseconds()}
		if !operation.IsZero() {
			metrics["queue_us"] = operation.Sub(start).Microseconds()
		}
		observation.Emit(ctx, observation.Event{Component: "ate", Name: name, Status: status, Metrics: metrics})
	}
	emit("lease.queued", "")
	return func() { operation = time.Now(); emit("lease.acquired", "") }, func(err error) {
		status := "ok"
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			status = "timeout"
		case errors.Is(err, context.Canceled):
			status = "canceled"
		case err != nil:
			status = "failed"
		}
		emit("lease.finished", status)
	}
}

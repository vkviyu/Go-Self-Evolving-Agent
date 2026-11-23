package ateprocess

import (
	"context"
	"sync/atomic"
	"time"
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
	if observer == nil {
		return func(Stage) {}
	}
	id := nextBatchID.Add(1)
	return func(stage Stage) {
		observer(ctx, Progress{BatchID: id, Stage: stage, At: time.Now()})
	}
}

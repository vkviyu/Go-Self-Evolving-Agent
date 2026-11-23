package cli

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateprocess"
	"github.com/vkviyu/go-self-evolving-agent/internal/tui"
)

func TestStartupPublisherForwardsTypedProgressAndClosesOnFailure(t *testing.T) {
	states := make(chan tui.StartupState, 8)
	publisher := &startupPublisher{states: states}
	publisher.phase(t.Context(), tui.StartupRootOwner)
	at := time.Now().Add(-time.Second)
	for _, stage := range []ateprocess.Stage{ateprocess.StageQueue, ateprocess.StageHandshake, ateprocess.StageDone} {
		publisher.observe(t.Context(), ateprocess.Progress{BatchID: 7, Stage: stage, At: at})
	}
	failure := errors.New("root recovery failed")
	publisher.finish(t.Context(), tui.StartupState{Err: failure})
	first := <-states
	if first.Phase != tui.StartupRootOwner || first.At.IsZero() {
		t.Fatalf("phase missing timestamp: %+v", first)
	}
	for _, stage := range []ateprocess.Stage{ateprocess.StageQueue, ateprocess.StageHandshake, ateprocess.StageDone} {
		state := <-states
		if state.Storage == nil || state.Storage.Stage != stage || state.Storage.BatchID != 7 || !state.Storage.At.Equal(at) {
			t.Fatalf("storage metadata not forwarded: %+v", state)
		}
	}
	if last := <-states; !errors.Is(last.Err, failure) || last.At.IsZero() || last.Ready {
		t.Fatalf("failure terminal state incorrect: %+v", last)
	}
	if _, ok := <-states; ok {
		t.Fatal("startup channel not closed")
	}
	// Late concurrent observers must be harmless after the final attachment.
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() { publisher.observe(t.Context(), ateprocess.Progress{Stage: ateprocess.StageQueue}) })
	}
	workers.Wait()
	publisher.finish(t.Context(), tui.StartupState{Ready: true})
}

func TestStartupPublisherObserverDoesNotWaitForUI(t *testing.T) {
	publisher := &startupPublisher{states: make(chan tui.StartupState)}
	done := make(chan struct{})
	go func() {
		publisher.observe(t.Context(), ateprocess.Progress{Stage: ateprocess.StageQueue})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("storage observer waited for UI")
	}
}

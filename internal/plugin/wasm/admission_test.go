package wasm

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRuntimeCallAdmissionBoundsConcurrentGuests(t *testing.T) {
	runtime := &Runtime{slots: make(chan struct{}, 2), done: make(chan struct{})}
	var running, peak atomic.Int32
	var workers sync.WaitGroup
	for range 20 {
		workers.Go(func() {
			if err := runtime.acquire(context.Background()); err != nil {
				t.Error(err)
				return
			}
			current := running.Add(1)
			for old := peak.Load(); current > old && !peak.CompareAndSwap(old, current); old = peak.Load() {
			}
			time.Sleep(time.Millisecond)
			running.Add(-1)
			<-runtime.slots
		})
	}
	workers.Wait()
	if peak.Load() > 2 || peak.Load() < 1 {
		t.Fatalf("concurrent admission peak = %d", peak.Load())
	}
}

func TestInvocationTimeoutIncludesWaitingForSlot(t *testing.T) {
	runtime := &Runtime{slots: make(chan struct{}, 1), done: make(chan struct{}), blobs: newBlobStore(16, 32), defaultTimeout: 10 * time.Millisecond}
	runtime.slots <- struct{}{}
	module := &Module{owner: runtime}
	start := time.Now()
	if _, err := module.Invoke(context.Background(), []byte(`{}`)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued call lost timeout: %v", err)
	}
	if time.Since(start) > time.Second || len(runtime.blobs.calls) != 0 {
		t.Fatal("queued call allocated guest resources or ignored timeout")
	}
}

func TestClosingRuntimeCancelsQueuedCalls(t *testing.T) {
	runtime, err := NewRuntime(context.Background(), Config{MaxConcurrentCalls: 1})
	if err != nil {
		t.Fatal(err)
	}
	runtime.slots <- struct{}{}
	done := make(chan error, 1)
	go func() { done <- runtime.acquire(context.Background()) }()
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrRuntimeClosed) {
			t.Fatalf("closed runtime admitted waiter: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runtime close did not wake queued call")
	}
}

func TestRuntimeLimitsAndGuestErrorAreBounded(t *testing.T) {
	for _, cfg := range []Config{{MaxConcurrentCalls: -1}, {MaxConcurrentCalls: 65}, {MemoryLimitPages: 65537}} {
		if runtime, err := NewRuntime(context.Background(), cfg); err == nil {
			_ = runtime.Close(context.Background())
			t.Fatalf("invalid resource limit accepted: %+v", cfg)
		}
	}
	if message := boundedGuestError(strings.Repeat("界", maxGuestErrorBytes)); len(message) > maxGuestErrorBytes+20 || !strings.HasSuffix(message, "[truncated]") {
		t.Fatalf("guest error was not bounded: %d bytes", len(message))
	}
}

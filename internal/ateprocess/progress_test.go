//go:build darwin || linux

package ateprocess

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/go-self-evolving-agent/internal/observation"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimeowner"
)

// TestProgressReportsBlockedWaits checks the visible stage while work is still
// blocked, rather than merely checking a list after the batch has finished.
func TestProgressReportsBlockedWaits(t *testing.T) {
	for _, stage := range []Stage{StageQueue, StageOwnership, StageHandshake} {
		t.Run(string(stage), func(t *testing.T) {
			mode := "normal"
			if stage == StageHandshake {
				mode = "hello-hang"
			}
			m := helperManager(t, mode, 5*time.Second)
			if stage == StageQueue {
				<-m.gate
				defer func() { m.gate <- struct{}{} }()
			}
			if stage == StageOwnership {
				lock, err := runtimeowner.Acquire(m.options.Directory)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			events := make(chan Progress, 32)
			ctx = WithObserver(ctx, func(_ context.Context, event Progress) { events <- event })
			done := make(chan error, 1)
			go func() {
				done <- m.WithClient(ctx, func(*client.Client) error {
					return errors.New("callback must not run")
				})
			}()
			var id uint64
			for {
				select {
				case event := <-events:
					id = event.BatchID
					if event.At.IsZero() || id == 0 {
						t.Fatal("missing timing identity")
					}
					if event.Stage != stage {
						continue
					}
				case err := <-done:
					t.Fatalf("batch ended before reporting blocked stage: %v", err)
				case <-time.After(3 * time.Second):
					t.Fatal("blocked stage was not reported")
				}
				break
			}
			select {
			case err := <-done:
				t.Fatalf("batch did not remain blocked: %v", err)
			default:
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation lost: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("cancellation did not release batch")
			}
			last := Progress{}
			for len(events) > 0 {
				last = <-events
			}
			if last.Stage != StageDone || last.BatchID != id {
				t.Fatalf("missing matching completion: %+v", last)
			}
		})
	}
}

func TestProgressBracketsCallbackAndShutdown(t *testing.T) {
	m := helperManager(t, "normal", 5*time.Second)
	var events []Progress
	ctx := WithObserver(context.Background(), func(_ context.Context, event Progress) {
		events = append(events, event)
	})
	sentinel := errors.New("callback result")
	err := m.WithClient(ctx, func(*client.Client) error {
		if events[len(events)-1].Stage != StageOperation {
			t.Error("operation was not reported before callback")
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("observation changed outcome: %v", err)
	}
	last := events[len(events)-1]
	if last.Stage != StageDone || events[len(events)-2].Stage != StageShutdown {
		t.Fatalf("batch was not bracketed through shutdown: %+v", events)
	}
	for i, event := range events {
		if event.BatchID != last.BatchID || (i > 0 && event.At.Before(events[i-1].At)) {
			t.Fatalf("invalid timing metadata: %+v", events)
		}
	}
	assertReapedAndUnlocked(t, m, ".launch")
}

func TestPersistentLeaseTraceSeparatesQueueFromOperation(t *testing.T) {
	r, err := observation.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	ctx := observation.WithScope(r.Attach(t.Context()), observation.Scope{RequestID: "model-request"})
	m := helperManager(t, "normal", 5*time.Second)
	lease, err := m.OpenClient(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := lease.WithClient(ctx, func(*client.Client) error { return nil }); err != nil {
		t.Fatal(err)
	}
	<-lease.gate
	defer func() { lease.gate <- struct{}{} }()
	timeout, cancel := context.WithTimeout(ctx, 15*time.Millisecond)
	defer cancel()
	if err := lease.WithClient(timeout, func(*client.Client) error { t.Fatal("unexpected callback while gate held"); return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(r.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	batches := map[int64][]string{}
	for {
		var e observation.Event
		if err := dec.Decode(&e); err != nil {
			if err != io.EOF {
				t.Fatal(err)
			}
			break
		}
		if e.Name == "lease.queued" || e.Name == "lease.acquired" || e.Name == "lease.finished" {
			if e.RequestID != "model-request" {
				t.Fatal(e)
			}
			batch := e.Metrics["batch"]
			batches[batch] = append(batches[batch], e.Name+e.Status)
		}
	}
	ok, timeoutSeen := false, false
	for _, events := range batches {
		if slices.Equal(events, []string{"lease.queued", "lease.acquired", "lease.finishedok"}) {
			ok = true
		}
		if slices.Equal(events, []string{"lease.queued", "lease.finishedtimeout"}) {
			timeoutSeen = true
		}
	}
	if len(batches) != 2 || !ok || !timeoutSeen {
		t.Fatal(batches)
	}
}

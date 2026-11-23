package atebridge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateprocess"
)

var _ Process = (*ateprocess.Manager)(nil)

func TestNativeTimeoutDuringMetadataKeepsGate(t *testing.T) {
	f := newFixture(t)
	f.useNative(t)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	f.p.check = func(request api.CommitRequest) error {
		if request.RequestID == f.b.requestID("namespace-binding") {
			close(entered)
			<-release
		}
		return nil
	}
	f.b.process = earlyReturnProcess{inner: f.p, entered: entered, done: done}
	request := stateRequest(t, f.b, "after metadata timeout")
	build := func(*client.Client) (api.CommitRequest, error) { return request, nil }
	_, err := f.b.Execute(t.Context(), "late-metadata", build)
	if !errors.Is(err, context.DeadlineExceeded) {
		close(release)
		<-done
		t.Fatalf("metadata did not time out: %v", err)
	}
	options := f.nativeOptions(t)
	options.Process = &pipeProcess{store: f.p.store}
	other, err := New(options)
	if err != nil {
		close(release)
		<-done
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	err = other.WithClient(ctx, func(*client.Client) error {
		t.Error("read lease entered while timed-out metadata still owned the gate")
		return nil
	})
	close(release)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("metadata timeout released mutation gate: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	f.b.process = f.p
	f.p.check = nil
	_, err = f.b.Execute(t.Context(), "late-metadata", func(*client.Client) (api.CommitRequest, error) {
		t.Error("rebuilt late command after metadata recovery")
		return request, nil
	})
	if err != nil || len(f.pendingRequests(t)) != 0 {
		t.Fatalf("late metadata/request recovery: %v", err)
	}
}

// Model the manager's documented timeout: the caller returns while an entered
// callback continues unwinding. The inner transport and backend are still real.
type earlyReturnProcess struct {
	inner   Process
	entered <-chan struct{}
	done    chan error
}

func (p earlyReturnProcess) WithClient(ctx context.Context, fn func(*client.Client) error) error {
	go func() { p.done <- p.inner.WithClient(ctx, fn) }()
	<-p.entered
	return context.DeadlineExceeded
}

func TestTimedOutCallbackRetainsGateAndOwnsResult(t *testing.T) {
	f := newFixture(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	f.b.process = earlyReturnProcess{inner: f.p, entered: entered, done: done}
	request := stateRequest(t, f.b, "late durable write")
	_, err := f.b.Execute(context.Background(), "late", func(*client.Client) (api.CommitRequest, error) {
		close(entered)
		<-release
		return request, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("early return: %v", err)
	}
	// A distinct process can connect concurrently, but its builder must not
	// enter while the original callback still owns the namespace.
	otherProcess := &pipeProcess{store: f.p.store}
	other, err := New(Options{Process: otherProcess, Pending: f.b.pending, State: f.b.state, Namespace: f.b.namespace, OperationalSpace: f.b.OperationalSpace()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = other.Execute(ctx, "second", func(*client.Client) (api.CommitRequest, error) {
		t.Error("timed-out callback released gate before returning")
		return request, nil
	})
	close(release)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("queued cancellation: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	f.b.process = f.p
	ack, err := f.b.Execute(context.Background(), "late", func(*client.Client) (api.CommitRequest, error) {
		t.Error("late commit rebuilt")
		return request, nil
	})
	if err != nil || ack.Code != api.CodeDurableAck {
		t.Fatalf("late receipt: %+v %v", ack, err)
	}
}

type delayedEntryProcess struct {
	callback func(*client.Client) error
}

func (p *delayedEntryProcess) WithClient(_ context.Context, fn func(*client.Client) error) error {
	p.callback = fn
	return context.DeadlineExceeded
}

func TestExpiredLeaseCannotEnterAfterGateReleased(t *testing.T) {
	f := newFixture(t)
	process := &delayedEntryProcess{}
	f.b.process = process
	if err := f.b.WithClient(t.Context(), func(*client.Client) error {
		t.Error("expired lease invoked caller")
		return nil
	}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	// A delayed callback must fail before inspecting even the client identity.
	if err := process.callback(nil); err == nil {
		t.Fatal("late callback entered an expired lease")
	}
	f.b.process = f.p
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := f.b.WithClient(ctx, func(*client.Client) error { return nil }); err != nil {
		t.Fatalf("expired lease kept the gate: %v", err)
	}
}

func TestCallbackPanicReleasesGate(t *testing.T) {
	f := newFixture(t)
	func() {
		defer func() {
			if got := recover(); got != "callback panic" {
				t.Errorf("callback panic changed: %v", got)
			}
		}()
		_ = f.b.WithClient(t.Context(), func(*client.Client) error { panic("callback panic") })
	}()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := f.b.WithClient(ctx, func(*client.Client) error { return nil }); err != nil {
		t.Fatalf("panic kept the gate: %v", err)
	}
}

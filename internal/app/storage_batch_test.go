package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateprocess"
	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	sessionate "github.com/vkviyu/go-self-evolving-agent/internal/session/atestore"
)

func storageBatchApp(t *testing.T) *App {
	t.Helper()
	binary := os.Getenv("GSEA_ATE_BINARY")
	if binary == "" {
		binary = testATEBinary(t)
	}
	cfg := config.Config{ATE: config.ATEConfig{
		Enabled: true, Binary: binary, Directory: filepath.Join(t.TempDir(), "ate"), Timeout: 10 * time.Second,
	}}
	db, err := runtimestore.OpenRuntime(t.Context(), filepath.Join(t.TempDir(), "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	authority, err := openAuthority(t.Context(), cfg, db.ATERequests())
	if err != nil {
		t.Fatal(err)
	}
	shutdown, cancel := context.WithCancel(context.Background())
	a := &App{
		authority: authority, database: db, temporalProcess: authority.process, config: cfg,
		evolutionContext: shutdown, evolutionCancel: cancel,
	}
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Error(err)
		}
	})
	return a
}

func storageBatchObserved(ctx context.Context) (context.Context, *atomic.Int64) {
	starts := new(atomic.Int64)
	return ateprocess.WithObserver(ctx, func(_ context.Context, progress ateprocess.Progress) {
		if progress.Stage == ateprocess.StageStart {
			starts.Add(1)
		}
	}), starts
}

func storageBatchRead(ctx context.Context, p authorityProcess) error {
	return p.WithClient(ctx, func(c *client.Client) error {
		_, err := c.Snapshot(ctx)
		return err
	})
}

func TestStorageBatchNativeReuseAndVisibility(t *testing.T) {
	a := storageBatchApp(t)
	ctx, starts := storageBatchObserved(t.Context())
	for range 6 {
		if err := storageBatchRead(ctx, a.authority.bound); err != nil {
			t.Fatal(err)
		}
	}
	if starts.Load() != 0 {
		t.Fatalf("persistent standalone launches = %d, want 0", starts.Load())
	}
	starts.Store(0)
	var leaked, nested context.Context
	var first *client.Client
	state := authorityState{records: a.authority.records}
	err := a.WithStorageBatch(ctx, func(ctx context.Context) error {
		leaked = context.WithoutCancel(ctx)
		// Setup methods acquire stateMu; the batch must not hold it.
		a.stateMu.Lock()
		a.stateMu.Unlock()
		for range 6 {
			if err := a.authority.bound.WithClient(ctx, func(c *client.Client) error {
				if first != nil && first != c {
					return errors.New("client changed inside scope")
				}
				first = c
				return nil
			}); err != nil {
				return err
			}
		}
		if _, err := a.authority.sessions.Append(ctx, "batch-session", 0, session.PendingEvent{
			Type: session.EventSessionCreated, Payload: json.RawMessage(`{"content":"batch"}`),
		}); err != nil {
			return err
		}
		if err := state.Put(ctx, "batch-state", json.RawMessage(`{"value":1}`)); err != nil {
			return err
		}
		events, err := a.authority.sessions.Load(ctx, "batch-session", 0, 10)
		if err != nil || len(events) != 1 {
			return errors.Join(err, errors.New("session not visible inside scope"))
		}
		if err := a.WithStorageBatch(ctx, func(ctx context.Context) error {
			nested = context.WithoutCancel(ctx)
			value, found, err := state.Get(ctx, "batch-state")
			if err != nil || !found || string(value) != `{"value":1}` {
				return errors.Join(err, errors.New("state not visible in nested scope"))
			}
			return nil
		}); err != nil {
			return err
		}
		if err := storageBatchRead(nested, a.authority.bound); !errors.Is(err, aterequest.ErrLeaseClosed) {
			return errors.New("nested context outlived callback")
		}
		return storageBatchRead(ctx, a.authority.bound)
	})
	if err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 0 {
		t.Fatalf("persistent batched launches = %d, want 0", starts.Load())
	}
	if err := storageBatchRead(leaked, a.authority.bound); !errors.Is(err, aterequest.ErrLeaseClosed) {
		t.Fatalf("leaked context accepted: %v", err)
	}
	if err := a.WithStorageBatch(leaked, func(context.Context) error {
		t.Error("leaked batch callback invoked")
		return nil
	}); !errors.Is(err, aterequest.ErrLeaseClosed) {
		t.Fatalf("leaked nested batch accepted: %v", err)
	}
	if starts.Load() != 0 {
		t.Fatal("leaked context launched another process")
	}
	events, err := a.authority.sessions.Load(ctx, "batch-session", 0, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("persisted session: count=%d err=%v", len(events), err)
	}
	value, found, err := state.Get(ctx, "batch-state")
	if err != nil || !found || string(value) != `{"value":1}` {
		t.Fatalf("persisted state: found=%v value=%s err=%v", found, value, err)
	}
	t.Log("six standalone reads and mixed scoped work reused the persistent subprocess")
}

func TestStorageBatchRejectsForeignAuthority(t *testing.T) {
	a := storageBatchApp(t)
	other, err := ateprocess.New(ateprocess.Options{
		Binary: a.config.ATE.Binary, Directory: a.config.ATE.Directory, Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	ctx, starts := storageBatchObserved(t.Context())
	err = a.WithStorageBatch(ctx, func(ctx context.Context) error {
		for _, p := range []authorityProcess{
			{manager: other, storeID: a.authority.storeID},
			{manager: a.authority.process, storeID: "another-store"},
			{manager: a.authority.process},
		} {
			err := p.WithClient(ctx, func(*client.Client) error {
				t.Error("foreign authority invoked callback")
				return nil
			})
			if !errors.Is(err, aterequest.ErrConflict) {
				return errors.New("foreign binding not rejected")
			}
		}
		return storageBatchRead(ctx, a.authority.bound)
	})
	if err != nil || starts.Load() != 0 {
		t.Fatalf("foreign authority: launches=%d err=%v", starts.Load(), err)
	}
}

func TestStorageBatchCancellationAndExpiration(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline", "manager-close"} {
		t.Run(mode, func(t *testing.T) {
			a := storageBatchApp(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx, starts := storageBatchObserved(ctx)
			err := a.WithStorageBatch(ctx, func(ctx context.Context) error {
				switch mode {
				case "cancel":
					cancel()
				case "deadline":
					// Expire the actual scope, not a child request context.
					// The outer timeout is covered separately below.
					scope := ctx.Value(storageBatchKey{}).(*storageBatchScope)
					scope.lease.cancel(context.DeadlineExceeded)
				case "manager-close":
					go a.authority.process.Close()
					select {
					case <-ctx.Done():
					case <-time.After(3 * time.Second):
						return errors.New("manager close did not cancel joined callback")
					}
				}
				fresh, stop := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
				defer stop()
				start := time.Now()
				err := storageBatchRead(fresh, a.authority.bound)
				if !errors.Is(err, aterequest.ErrLeaseClosed) || time.Since(start) > 500*time.Millisecond {
					return errors.New("fresh reconciliation did not reject expired lease promptly")
				}
				return err
			})
			if !errors.Is(err, aterequest.ErrLeaseClosed) || starts.Load() != 0 {
				t.Fatalf("expired batch: launches=%d err=%v", starts.Load(), err)
			}
			if mode != "manager-close" {
				if err := storageBatchRead(t.Context(), a.authority.bound); err != nil {
					t.Fatalf("independent recovery failed: %v", err)
				}
			}
		})
	}
}

func TestStorageBatchNativeDeadline(t *testing.T) {
	a := storageBatchApp(t)
	a.config.ATE.Timeout = 500 * time.Millisecond
	called := false
	err := a.WithStorageBatch(t.Context(), func(ctx context.Context) error {
		called = true
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
			return errors.New("storage scope has no deadline")
		}
		return storageBatchRead(context.WithoutCancel(ctx), a.authority.bound)
	})
	if !called || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, aterequest.ErrLeaseClosed) {
		t.Fatalf("native deadline: called=%v err=%v", called, err)
	}
	if err := storageBatchRead(t.Context(), a.authority.bound); err != nil {
		t.Fatal(err)
	}
}

func TestStorageBatchConcurrentAndRecursiveCallbacks(t *testing.T) {
	a := storageBatchApp(t)
	err := a.WithStorageBatch(t.Context(), func(ctx context.Context) error {
		entered, release := make(chan struct{}), make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- a.authority.bound.WithClient(ctx, func(*client.Client) error {
				close(entered)
				<-release
				return nil
			})
		}()
		<-entered
		err := storageBatchRead(ctx, a.authority.bound)
		close(release)
		if firstErr := <-done; firstErr != nil {
			return firstErr
		}
		if !errors.Is(err, ErrStorageBatchBusy) {
			return errors.New("concurrent callback was not rejected")
		}
		if err := a.authority.bound.WithClient(ctx, func(*client.Client) error {
			if err := storageBatchRead(ctx, a.authority.bound); !errors.Is(err, ErrStorageBatchBusy) {
				return errors.New("recursive callback was not rejected")
			}
			if err := a.WithStorageBatch(ctx, func(context.Context) error {
				t.Error("recursive batch invoked")
				return nil
			}); !errors.Is(err, ErrStorageBatchBusy) {
				return errors.New("recursive batch was not rejected")
			}
			return nil
		}); err != nil {
			return err
		}
		return storageBatchRead(ctx, a.authority.bound)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStorageBatchFailureDoesNotReplayOrRollback(t *testing.T) {
	a := storageBatchApp(t)
	ctx, starts := storageBatchObserved(t.Context())
	injected := errors.New("stop after first commit")
	calls := 0
	err := a.WithStorageBatch(ctx, func(ctx context.Context) error {
		calls++
		if _, err := a.authority.sessions.Append(ctx, "partial", 0, session.PendingEvent{
			Type: session.EventSessionCreated, Payload: json.RawMessage(`{}`),
		}); err != nil {
			return err
		}
		return injected
	})
	if !errors.Is(err, injected) || calls != 1 || starts.Load() != 0 {
		t.Fatalf("callback replayed: calls=%d launches=%d err=%v", calls, starts.Load(), err)
	}
	events, err := a.authority.sessions.Load(t.Context(), "partial", 0, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("prior commit was lost or duplicated: count=%d err=%v", len(events), err)
	}
}

func TestStorageBatchUnknownDefersFreshReconciliation(t *testing.T) {
	a := storageBatchApp(t)
	pending := a.database.ATERequests()
	writer, err := sessionate.New(sessionate.Options{
		Process: a.authority.bound, Pending: pending, Space: authoritySpace, StoreID: a.authority.storeID,
		ReconciliationTimeout: 5 * time.Second,
		NewExecutor: func(c *client.Client, id model.StoreID) (sessionate.Executor, error) {
			return aterequest.NewExecutor(&startupLostReply{Client: c, mode: "append-after"}, pending, id)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, starts := storageBatchObserved(t.Context())
	calls := 0
	err = a.WithStorageBatch(ctx, func(ctx context.Context) error {
		calls++
		_, err := writer.Append(ctx, "unknown", 0, session.PendingEvent{
			Type: session.EventSessionCreated, Payload: json.RawMessage(`{}`),
		})
		return err
	})
	if !errors.Is(err, aterequest.ErrUnknown) || !errors.Is(err, aterequest.ErrLeaseClosed) ||
		calls != 1 || starts.Load() != 0 {
		t.Fatalf("unknown reconciliation: calls=%d launches=%d err=%v", calls, starts.Load(), err)
	}
	records, err := pending.List(t.Context(), aterequest.ListOptions{Limit: 100, MaxBytes: aterequest.MaxListBytes})
	if err != nil || len(records) == 0 {
		t.Fatalf("original request not retained: count=%d err=%v", len(records), err)
	}
	if err := a.authority.sessions.RecoverSession(t.Context(), "unknown"); err != nil {
		t.Fatalf("independent recovery: %v", err)
	}
	events, err := a.authority.sessions.Load(t.Context(), "unknown", 0, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("recovery duplicated or lost append: count=%d err=%v", len(events), err)
	}
	records, err = pending.List(t.Context(), aterequest.ListOptions{Limit: 100, MaxBytes: aterequest.MaxListBytes})
	if err != nil || len(records) != 0 {
		t.Fatalf("recovery retained pending request: count=%d err=%v", len(records), err)
	}
}

func TestStorageBatchCloseCancelsAndJoins(t *testing.T) {
	a := storageBatchApp(t)
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- a.WithStorageBatch(t.Context(), func(ctx context.Context) error {
			a.stateMu.Lock()
			a.stateMu.Unlock()
			close(entered)
			<-ctx.Done()
			close(canceled)
			<-release
			// Close must keep the pending database alive until run unwinds.
			_, err := a.database.ATERequests().List(context.Background(), aterequest.ListOptions{
				Limit: 1, MaxBytes: aterequest.MaxListBytes,
			})
			if err != nil {
				return err
			}
			return ctx.Err()
		})
	}()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- a.Close() }()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("Close failed to cancel storage scope")
	}
	select {
	case err := <-closed:
		t.Fatalf("Close returned before callback unwound: %v", err)
	default:
	}
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("batch cancellation: %v", err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if err := a.WithStorageBatch(t.Context(), func(context.Context) error {
		t.Error("closed App invoked batch")
		return nil
	}); err == nil {
		t.Fatal("closed App accepted batch")
	}
}

func TestStorageBatchInvalidArguments(t *testing.T) {
	var a *App
	if err := a.WithStorageBatch(t.Context(), func(context.Context) error { return nil }); !errors.Is(err, aterequest.ErrInvalid) {
		t.Fatal(err)
	}
	a = &App{}
	for _, err := range []error{
		a.WithStorageBatch(nil, func(context.Context) error { return nil }),
		a.WithStorageBatch(t.Context(), nil),
		a.WithStorageBatch(t.Context(), func(context.Context) error { return nil }),
	} {
		if !errors.Is(err, aterequest.ErrInvalid) {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := a.WithStorageBatch(ctx, func(context.Context) error {
		t.Error("canceled request invoked callback")
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestStorageBatchDrainsInFlightCallback(t *testing.T) {
	a := storageBatchApp(t)
	entered, closing, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	callbackDone := make(chan error, 1)
	batchDone := make(chan error, 1)
	var leaked context.Context
	go func() {
		batchDone <- a.WithStorageBatch(t.Context(), func(ctx context.Context) error {
			leaked = context.WithoutCancel(ctx)
			go func() {
				callbackDone <- a.authority.bound.WithClient(ctx, func(*client.Client) error {
					close(entered)
					<-ctx.Done() // run has returned and scope.close is draining.
					close(closing)
					<-release
					return nil
				})
			}()
			<-entered
			return nil
		})
	}()
	<-closing
	select {
	case err := <-batchDone:
		t.Fatalf("batch returned with active client callback: %v", err)
	default:
	}
	if err := storageBatchRead(leaked, a.authority.bound); !errors.Is(err, aterequest.ErrLeaseClosed) {
		t.Fatalf("closing scope admitted callback: %v", err)
	}
	close(release)
	if err := <-callbackDone; err != nil {
		t.Fatal(err)
	}
	if err := <-batchDone; err != nil {
		t.Fatal(err)
	}
	if err := storageBatchRead(t.Context(), a.authority.bound); err != nil {
		t.Fatal(err)
	}
}

func TestStorageBatchCanceledRequestInvalidatesLease(t *testing.T) {
	a := storageBatchApp(t)
	err := a.WithStorageBatch(t.Context(), func(ctx context.Context) error {
		request, cancel := context.WithCancel(ctx)
		defer cancel()
		err := a.authority.bound.WithClient(request, func(*client.Client) error {
			cancel()
			return request.Err()
		})
		if !errors.Is(err, context.Canceled) {
			return errors.New("request cancellation was lost")
		}
		err = storageBatchRead(context.WithoutCancel(request), a.authority.bound)
		if !errors.Is(err, aterequest.ErrLeaseClosed) {
			return errors.New("fresh reconciliation reused canceled request client")
		}
		return err
	})
	if !errors.Is(err, aterequest.ErrLeaseClosed) {
		t.Fatal(err)
	}
}

func TestStorageBatchClosedClientIsNotPassedToCallback(t *testing.T) {
	a := storageBatchApp(t)
	err := a.WithStorageBatch(t.Context(), func(ctx context.Context) error {
		if err := a.authority.bound.WithClient(ctx, func(c *client.Client) error { return c.Close() }); err != nil {
			return err
		}
		err := a.authority.bound.WithClient(ctx, func(*client.Client) error {
			t.Error("closed client reached callback")
			return nil
		})
		if !errors.Is(err, client.ErrClosed) {
			return errors.New("closed client was not detected")
		}
		if err := storageBatchRead(context.WithoutCancel(ctx), a.authority.bound); !errors.Is(err, aterequest.ErrLeaseClosed) {
			return errors.New("broken lease was not invalidated")
		}
		return err
	})
	if !errors.Is(err, client.ErrClosed) {
		t.Fatal(err)
	}
}

func TestStorageBatchControlAppAndIdentityValidation(t *testing.T) {
	a := storageBatchApp(t)
	cfg := a.config
	cfg.HomeDirectory, cfg.DataDirectory, cfg.WorkspaceDirectory = t.TempDir(), t.TempDir(), t.TempDir()
	if err := a.authority.process.Close(); err != nil {
		t.Fatal(err)
	}
	control, err := OpenControl(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if err := control.WithStorageBatch(t.Context(), func(ctx context.Context) error {
		return storageBatchRead(ctx, control.authority.bound)
	}); err != nil {
		t.Fatalf("control App storage scope: %v", err)
	}
	control.authority.bound.storeID = "wrong-store"
	err = control.WithStorageBatch(t.Context(), func(context.Context) error {
		t.Error("batch callback ran before store identity validation")
		return nil
	})
	if !errors.Is(err, aterequest.ErrConflict) {
		t.Fatalf("authority replacement was not rejected: %v", err)
	}
}

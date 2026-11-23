package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
)

// ErrStorageBatchBusy rejects concurrent or recursive client callbacks within
// one storage batch. The Process interface cannot pass a callback-local context
// to distinguish recursion from another goroutine using the same context.
var ErrStorageBatchBusy = errors.New("storage batch client callback is already active")

type storageBatchKey struct{}

type storageBatchLease struct {
	process authorityProcess
	client  *client.Client
	ctx     context.Context
	cancel  context.CancelCauseFunc
	gate    sync.Mutex
}

type storageBatchScope struct {
	lease  *storageBatchLease
	parent *storageBatchScope
	ctx    context.Context
	cancel context.CancelFunc
}

// WithStorageBatch runs bounded local storage setup using one authenticated ATE
// process. Pass the supplied context to all repository calls. The whole scope,
// including startup and ownership contention, is bounded by the ATE timeout.
//
// run is called at most once; this is not an atomic transaction and errors do
// not roll back earlier commits. Never retry the whole callback automatically.
// Do not perform model/network/user waits, start long-lived workers, retain the
// context, or call App.Close inside run. Use the original context for subsequent
// execution and recovery. Close cancels and joins the scope as owned App work.
//
// Nested batches share the lease but have their own context lifetime. Client
// callbacks are exclusive: overlapping or recursive WithClient calls fail with
// ErrStorageBatchBusy, rather than waiting on a potentially recursive lock.
// Unknown outcomes invalidate the lease. Fresh reconciliation through a
// WithoutCancel context then returns aterequest.ErrLeaseClosed; recovery must
// occur outside the batch, after the native owner has been released.
func (a *App) WithStorageBatch(ctx context.Context, run func(context.Context) error) error {
	if a == nil || ctx == nil || run == nil {
		return aterequest.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	a.stateMu.Lock()
	if a.closed {
		a.stateMu.Unlock()
		return errors.New("application is closed")
	}
	authority := a.authority
	if authority == nil || authority.bound.manager == nil || authority.storeID == "" {
		a.stateMu.Unlock()
		return aterequest.ErrInvalid
	}
	// Register under the same lock as Close's closed flag. Close already cancels
	// and joins this owned-work group before disposing of any runtime resources.
	// Read-only control Apps initialize this lifecycle lazily.
	if a.evolutionContext == nil {
		a.evolutionContext, a.evolutionCancel = context.WithCancel(context.Background())
	}
	a.evolutionWork.Add(1)
	shutdown := a.evolutionContext
	timeout := a.config.ATE.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second // Same default as ateprocess.New.
	}
	a.stateMu.Unlock()
	defer a.evolutionWork.Done()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stop := context.AfterFunc(shutdown, cancel)
	defer stop()
	if shutdown.Err() != nil {
		cancel()
	}
	if parent, ok := ctx.Value(storageBatchKey{}).(*storageBatchScope); ok {
		if err := parent.check(ctx, authority.bound); err != nil {
			return err
		}
		if !parent.lease.gate.TryLock() {
			return ErrStorageBatchBusy
		}
		parent.lease.gate.Unlock()
		// WithoutCancel may hide the parent scope cancellation. Keep the
		// explicit lifetime dependency even when the caller removed it.
		stop := context.AfterFunc(parent.ctx, cancel)
		defer stop()
		return runStorageBatch(ctx, parent.lease, parent, run)
	}

	if lease := authority.bound.currentLease(); lease != nil {
		err := lease.WithClientContext(ctx, func(operation context.Context, c *client.Client) error {
			if err := authority.bound.validateClient(operation, c); err != nil {
				return err
			}
			leaseCtx, leaseCancel := context.WithCancelCause(operation)
			defer leaseCancel(aterequest.ErrLeaseClosed)
			lease := &storageBatchLease{process: authority.bound, client: c, ctx: leaseCtx, cancel: leaseCancel}
			return runStorageBatch(leaseCtx, lease, nil, run)
		})
		if errors.Is(err, aterequest.ErrUnknown) {
			lease.Invalidate()
			return errors.Join(err, aterequest.ErrLeaseClosed)
		}
		return err
	}
	// Keep the short-lived fallback for tests and callers that provide a
	// process without an opened persistent lease.
	return aterequest.WithClient(ctx, authority.bound.manager, func(operation context.Context, c *client.Client) error {
		if err := authority.bound.validateClient(operation, c); err != nil {
			return err
		}
		leaseCtx, leaseCancel := context.WithCancelCause(ctx)
		defer leaseCancel(aterequest.ErrLeaseClosed)
		lease := &storageBatchLease{process: authority.bound, client: c, ctx: leaseCtx, cancel: leaseCancel}
		return runStorageBatch(leaseCtx, lease, nil, run)
	})
}

func runStorageBatch(ctx context.Context, lease *storageBatchLease, parent *storageBatchScope, run func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	scope := &storageBatchScope{lease: lease, parent: parent, ctx: ctx, cancel: cancel}
	defer scope.close()
	if err := scope.check(ctx, lease.process); err != nil {
		return err
	}
	batchContext := context.WithValue(ctx, storageBatchKey{}, scope)
	batchContext = aterequest.DeferReconciliation(batchContext)
	err := run(batchContext)
	if err != nil {
		return err
	}
	return scope.check(ctx, lease.process)
}

func (s *storageBatchScope) check(ctx context.Context, p authorityProcess) error {
	if s.lease.process.manager != p.manager || s.lease.process.storeID != p.storeID {
		// Never fall back to another manager: it may target the same directory
		// and wait forever for the owner held by this scope.
		return aterequest.ErrConflict
	}
	if s.ctx.Err() != nil || s.lease.ctx.Err() != nil {
		return errors.Join(aterequest.ErrLeaseClosed, s.ctx.Err(), context.Cause(s.lease.ctx))
	}
	if s.parent != nil {
		return s.parent.check(ctx, p)
	}
	return ctx.Err()
}

func (s *storageBatchScope) withClient(ctx context.Context, p authorityProcess, fn func(*client.Client) error) error {
	if err := s.check(ctx, p); err != nil {
		return err
	}
	if !s.lease.gate.TryLock() {
		return ErrStorageBatchBusy
	}
	defer s.lease.gate.Unlock()
	if err := s.check(ctx, p); err != nil {
		return err
	}
	// Preserve the ordinary authority identity check and detect a broken
	// connection before invoking another repository callback.
	if err := p.validateClient(ctx, s.lease.client); err != nil {
		s.lease.cancel(err)
		return err
	}
	err := fn(s.lease.client)
	var unknown *client.OutcomeUnknownError
	if ctx.Err() != nil || errors.Is(err, aterequest.ErrUnknown) ||
		errors.Is(err, client.ErrClosed) || errors.As(err, &unknown) {
		s.lease.cancel(errors.Join(err, ctx.Err()))
	}
	return err
}

func (s *storageBatchScope) close() {
	// Reject admission before waiting for a callback that escaped run. Even a
	// WithoutCancel context cannot use this scope after its callback returns.
	s.cancel()
	s.lease.gate.Lock()
	s.lease.gate.Unlock()
}

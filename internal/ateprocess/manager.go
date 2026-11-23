// Package ateprocess owns authenticated native ATE subprocess lifetimes and
// bounded callbacks.
package ateprocess

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimeowner"
)

type (
	Options struct {
		Binary, Directory string
		Timeout           time.Duration
	}

	// Manager serializes local callbacks and owns the native ATE lifetime.
	Manager struct {
		options Options
		gate    chan struct{}
		ctx     context.Context
		cancel  context.CancelFunc
		mu      sync.Mutex
		closed  bool
		lease   *ClientLease
		active  sync.WaitGroup
	}

	// ClientLease owns one authenticated native ATE process, client and store
	// owner. Its callbacks are serialized and joined before another callback or
	// Close proceeds.
	ClientLease struct {
		manager    *Manager
		owner      *runtimeowner.Lock
		process    *process
		client     *client.Client
		ctx        context.Context
		cancel     context.CancelFunc
		gate       chan struct{}
		mu         sync.Mutex
		closed     bool
		activeDone chan struct{}
		closeOnce  sync.Once
		closeDone  chan struct{}
		closeErr   error
	}

	// batchError hides untrusted protocol messages while retaining typed errors
	// (especially OutcomeUnknownError and its original RequestKey) for callers.
	batchError struct {
		stage string
		cause error
	}

	callbackResult struct {
		err        error
		panicValue any
	}
)

var (
	ErrClosed      = errors.New("ATE subprocess manager is closed")
	ErrLeaseClosed = errors.New("ATE client lease is closed")
)

func (e *batchError) Error() string { return "ATE " + e.stage + " failed" }
func (e *batchError) Unwrap() error { return e.cause }

// New validates options without starting a process or creating the store.
// Zero Binary and Timeout select ate and 30s, respectively.
// At batch startup, ate prefers an executable beside this process, then PATH.
// Explicit binary paths retain their configured location.
func New(options Options) (*Manager, error) {
	if options.Binary == "" {
		options.Binary = "ate"
	}
	if options.Timeout == 0 {
		options.Timeout = 30 * time.Second
	}
	if options.Timeout < 0 || options.Timeout > 10*time.Minute {
		return nil, errors.New("ATE timeout must be positive and at most 10m")
	}
	for _, value := range []string{options.Binary, options.Directory} {
		if strings.TrimSpace(value) == "" || len(value) > 4096 || strings.ContainsAny(value, "\x00\r\n") {
			return nil, errors.New("ATE paths must be nonblank, at most 4096 bytes, and contain no NUL or line breaks")
		}
	}
	var err error
	options.Directory, err = filepath.Abs(options.Directory)
	if err != nil || len(options.Directory) > 4096 || options.Directory == filepath.Dir(options.Directory) {
		return nil, errors.New("ATE directory must be a bounded non-root path")
	}
	if strings.ContainsAny(options.Binary, `/\`) {
		options.Binary, err = filepath.Abs(options.Binary)
		if err != nil || len(options.Binary) > 4096 {
			return nil, errors.New("resolve ATE binary")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{options: options, gate: make(chan struct{}, 1), ctx: ctx, cancel: cancel}
	m.gate <- struct{}{}
	return m, nil
}

// OpenClient starts one long-lived authenticated native ATE process. The
// opening context bounds startup; the manager lifetime bounds the resulting
// lease. The returned client remains valid only inside lease callbacks.
func (m *Manager) OpenClient(ctx context.Context) (*ClientLease, error) {
	if ctx == nil {
		return nil, errors.New("ATE lease requires a context")
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrClosed
	}
	m.active.Add(1)
	m.mu.Unlock()
	success := false
	defer func() {
		if !success {
			m.active.Done()
		}
	}()

	ctx, cancel := context.WithTimeout(ctx, m.options.Timeout)
	defer cancel()
	stop := context.AfterFunc(m.ctx, cancel)
	defer stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.gate:
	}
	releaseGate := true
	defer func() {
		if releaseGate {
			m.gate <- struct{}{}
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	progress := batchProgress(ctx)
	defer progress(StageDone)
	owner, p, c, err := m.openNative(ctx, context.Background(), progress)
	if err != nil {
		return nil, err
	}
	leaseCtx, leaseCancel := context.WithCancel(m.ctx)
	lease := &ClientLease{
		manager: m, owner: owner, process: p, client: c,
		ctx: leaseCtx, cancel: leaseCancel,
		gate: make(chan struct{}, 1), closeDone: make(chan struct{}),
	}
	lease.gate <- struct{}{}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		success = true
		releaseGate = false
		_ = lease.Close()
		return nil, ErrClosed
	}
	m.lease = lease
	m.mu.Unlock()
	releaseGate = false
	success = true
	return lease, nil
}

// OpenLease is an alias for OpenClient for callers that model the connection
// as an explicit lifecycle lease.
func (m *Manager) OpenLease(ctx context.Context) (*ClientLease, error) {
	return m.OpenClient(ctx)
}

// CurrentLease returns the manager's current persistent lease. The lease is
// only valid inside its callback methods; callers must not retain the client.
func (m *Manager) CurrentLease() *ClientLease {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lease
}

// WithFreshClient closes the current persistent lease, if any, opens a new
// authenticated connection, and runs exactly one callback on it. Callers use
// this only for explicit reconciliation or recovery; ordinary WithClient must
// never reopen a connection and replay a business callback implicitly.
func (m *Manager) WithFreshClient(ctx context.Context, fn func(*client.Client) error) error {
	if ctx == nil || fn == nil {
		return errors.New("ATE fresh client requires a context and callback")
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	lease := m.lease
	m.mu.Unlock()
	if lease == nil {
		return m.WithClient(ctx, fn)
	}
	if err := lease.Close(); err != nil {
		return err
	}
	fresh, err := m.OpenClient(ctx)
	if err != nil {
		return err
	}
	return fresh.WithClient(ctx, fn)
}

// WithClient keeps the historical short-lived behavior when no persistent
// lease is open. Once a lease is open, old callers safely reuse that lease.
func (m *Manager) WithClient(ctx context.Context, fn func(*client.Client) error) (err error) {
	if ctx == nil || fn == nil {
		return errors.New("ATE batch requires a context and callback")
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	if lease := m.lease; lease != nil {
		m.mu.Unlock()
		return lease.WithClient(ctx, fn)
	}
	m.active.Add(1)
	m.mu.Unlock()
	defer m.active.Done()

	ctx, cancel := context.WithTimeout(ctx, m.options.Timeout)
	defer cancel()
	stop := context.AfterFunc(m.ctx, cancel)
	defer stop()
	progress := batchProgress(ctx)
	defer progress(StageDone)
	progress(StageQueue)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.gate:
	}
	var callbackDone <-chan struct{}
	defer func() {
		if callbackDone == nil {
			m.gate <- struct{}{}
		} else {
			go func() { <-callbackDone; m.gate <- struct{}{} }()
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	owner, p, c, err := m.openNative(ctx, ctx, progress)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := owner.Close(); closeErr != nil {
			err = errors.Join(err, &batchError{stage: "ownership release", cause: closeErr})
		}
	}()
	defer func() {
		defer p.stop()
		progress(StageShutdown)
		if closeErr := p.finish(); closeErr != nil {
			err = errors.Join(err, &batchError{stage: "subprocess shutdown", cause: closeErr})
		}
	}()
	defer func() {
		progress(StageShutdown)
		_ = c.Close()
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	progress(StageOperation)
	result := make(chan callbackResult, 1)
	done := make(chan struct{})
	callbackDone = done
	go func() {
		var r callbackResult
		defer func() {
			r.panicValue = recover()
			result <- r
			close(done)
		}()
		r.err = fn(c)
	}()
	select {
	case r := <-result:
		if r.panicValue != nil {
			panic(r.panicValue)
		}
		if r.err != nil {
			return &batchError{stage: "callback", cause: r.err}
		}
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WithClientContext runs one bounded, serialized callback and supplies the
// bounded operation context used by the lease. The callback is always joined
// before the lease admits another operation.
func (l *ClientLease) WithClientContext(ctx context.Context, fn func(context.Context, *client.Client) error) error {
	if l == nil || ctx == nil || fn == nil {
		return errors.New("ATE lease requires a context and callback")
	}
	opCtx, cancel := context.WithTimeout(ctx, l.manager.options.Timeout)
	defer cancel()
	stop := context.AfterFunc(l.ctx, cancel)
	defer stop()
	select {
	case <-opCtx.Done():
		return l.operationError(opCtx.Err())
	case <-l.gate:
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		l.gate <- struct{}{}
		return ErrLeaseClosed
	}
	if l.client.Closed() {
		l.closed = true
		l.cancel()
		l.mu.Unlock()
		_ = l.client.Close()
		l.gate <- struct{}{}
		return client.ErrClosed
	}
	done := make(chan struct{})
	l.activeDone = done
	c := l.client
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		if l.activeDone == done {
			l.activeDone = nil
		}
		l.mu.Unlock()
		close(done)
		l.gate <- struct{}{}
	}()

	result := make(chan callbackResult, 1)
	go func() {
		var r callbackResult
		defer func() {
			r.panicValue = recover()
			result <- r
		}()
		r.err = fn(opCtx, c)
	}()
	select {
	case r := <-result:
		if r.panicValue != nil {
			panic(r.panicValue)
		}
		if r.err != nil {
			if leaseBreaking(r.err) || c.Closed() {
				l.invalidate()
			}
			return r.err
		}
		if err := opCtx.Err(); err != nil {
			return err
		}
		return nil
	case <-opCtx.Done():
		if c.Closed() {
			l.invalidate()
		}
		r := <-result
		if r.panicValue != nil {
			panic(r.panicValue)
		}
		if r.err != nil {
			return errors.Join(opCtx.Err(), r.err)
		}
		return opCtx.Err()
	}
}

// WithClient runs a bounded callback on the persistent client.
func (l *ClientLease) WithClient(ctx context.Context, fn func(*client.Client) error) error {
	return l.WithClientContext(ctx, func(_ context.Context, c *client.Client) error {
		return fn(c)
	})
}

// Invalidate prevents further callbacks from using this connection. The
// process and owner remain held until Close so an explicit fresh connection can
// release them in a deterministic order.
func (l *ClientLease) Invalidate() {
	if l == nil {
		return
	}
	l.invalidate()
}

func (l *ClientLease) operationError(err error) error {
	l.mu.Lock()
	closed := l.closed
	l.mu.Unlock()
	if closed {
		return ErrLeaseClosed
	}
	return err
}

func leaseBreaking(err error) bool {
	var unknown *client.OutcomeUnknownError
	return errors.Is(err, client.ErrClosed) || errors.As(err, &unknown)
}

func (l *ClientLease) invalidate() {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.closed = true
	l.cancel()
	c := l.client
	l.mu.Unlock()
	_ = c.Close()
}

// Close cancels the lease, closes the native client, joins any callback, then
// joins the child process and releases store ownership. It is idempotent.
func (l *ClientLease) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	if !l.closed {
		l.closed = true
		l.cancel()
	}
	c := l.client
	activeDone := l.activeDone
	l.mu.Unlock()
	_ = c.Close()
	if activeDone != nil {
		<-activeDone
	}
	l.closeOnce.Do(func() {
		var errs []error
		if err := l.process.finish(); err != nil {
			errs = append(errs, &batchError{stage: "subprocess shutdown", cause: err})
		}
		l.process.stop()
		if err := l.owner.Close(); err != nil {
			errs = append(errs, &batchError{stage: "ownership release", cause: err})
		}
		l.manager.mu.Lock()
		if l.manager.lease == l {
			l.manager.lease = nil
		}
		l.manager.mu.Unlock()
		select {
		case l.manager.gate <- struct{}{}:
		default:
		}
		l.manager.active.Done()
		l.closeErr = errors.Join(errs...)
		close(l.closeDone)
	})
	<-l.closeDone
	return l.closeErr
}

func (m *Manager) openNative(ctx, processContext context.Context, progress func(Stage)) (owner *runtimeowner.Lock, p *process, c *client.Client, err error) {
	binary, err := resolveBinary(m.options.Binary, os.Executable, exec.LookPath)
	if err != nil {
		return nil, nil, nil, &batchError{stage: "binary resolution", cause: err}
	}
	progress(StageOwnership)
	owner, err = m.acquire(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	keep := false
	defer func() {
		if keep {
			return
		}
		if c != nil {
			_ = c.Close()
		}
		if p != nil {
			_ = p.cmd.Process.Kill()
			_ = p.finish()
			<-p.done
			p.stop()
		}
		if owner != nil {
			if closeErr := owner.Close(); closeErr != nil && err == nil {
				err = &batchError{stage: "ownership release", cause: closeErr}
			}
		}
	}()
	progress(StageInitialize)
	if err := m.initialize(ctx, binary); err != nil {
		return owner, p, c, err
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return owner, p, c, &batchError{stage: "credential generation", cause: err}
	}
	token := hex.EncodeToString(secret[:])
	progress(StageStart)
	p, err = m.start(processContext, binary, token)
	if err != nil {
		return owner, p, c, err
	}
	progress(StageHandshake)
	c, err = client.New(ctx, p.stream, client.Options{Token: token})
	if err != nil {
		return owner, p, c, &batchError{stage: "hello", cause: err}
	}
	if err := validateIdentity(c); err != nil {
		return owner, p, c, err
	}
	keep = true
	return owner, p, c, nil
}

func validateIdentity(c *client.Client) error {
	identity := c.Identity()
	if !identity.AllSpaces {
		return errors.New("ATE hello requires AllSpaces authorization")
	}
	for _, required := range []string{"agent/v1", "event/v1", "knowledge/v1", "memory/v1", "memory/query-v1", "space/catalog-v1", "space/organization-v1"} {
		found := false
		for _, capability := range identity.Capabilities {
			found = found || capability == required
		}
		if !found {
			return errors.New("ATE hello missing required capability: " + required)
		}
	}
	return nil
}

// Close cancels queued and active batches and waits until all children have
// exited and their locks have been released. It is safe to call repeatedly.
func (m *Manager) Close() error {
	m.mu.Lock()
	m.closed = true
	m.cancel()
	lease := m.lease
	m.mu.Unlock()
	if lease != nil {
		_ = lease.Close()
	}
	m.active.Wait()
	return nil
}

func (m *Manager) acquire(ctx context.Context) (*runtimeowner.Lock, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Acquire creates only the parent. Its canonical sibling lock stays
		// outside the store so init-state still sees an empty directory.
		lock, err := runtimeowner.Acquire(m.options.Directory)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, runtimeowner.ErrOwned) {
			return nil, &batchError{stage: "ownership acquisition", cause: err}
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (m *Manager) initialize(ctx context.Context, binary string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(m.options.Directory, 0700); err != nil {
		return &batchError{stage: "store directory creation", cause: err}
	}
	dir, err := os.Open(m.options.Directory)
	if err != nil {
		return &batchError{stage: "store inspection", cause: err}
	}
	names, readErr := dir.Readdirnames(1)
	closeErr := dir.Close()
	if len(names) != 0 {
		// Never initialize over an existing or partially initialized store.
		// serve owns validation and recovery of existing contents.
		return nil
	}
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return &batchError{stage: "store inspection", cause: readErr}
	}
	if closeErr != nil {
		return &batchError{stage: "store inspection", cause: closeErr}
	}
	return m.initProcess(ctx, binary)
}

// Package ateprocess owns authenticated native ATE subprocess lifetimes and
// bounded callbacks.
package ateprocess

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
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
		StartupTimeout    time.Duration // Native store replay and authenticated hello.
		// ExistingOnly skips store initialization and opens it with ate serve.
		ExistingOnly bool
		// NoWaitForOwner lets a catalog command report the active owner immediately.
		NoWaitForOwner bool
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

	// ClientLease holds the store owner for its lifetime. Its gate protects
	// the replaceable process/client pair and serializes callbacks and shutdown.
	ClientLease struct {
		manager   *Manager
		owner     *runtimeowner.Lock
		process   *process
		client    *client.Client
		ctx       context.Context
		cancel    context.CancelFunc
		gate      chan struct{}
		closeOnce sync.Once
		closeErr  error
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
	if options.StartupTimeout == 0 {
		options.StartupTimeout = 10 * time.Minute
	}
	if options.StartupTimeout < 0 {
		return nil, errors.New("ATE startup timeout must be positive")
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

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(m.ctx, cancel)
	defer stop()
	progress := batchProgress(ctx)
	defer progress(StageDone)
	progress(StageQueue)
	if err := waitGate(ctx, m.gate, m.options.Timeout); err != nil {
		return nil, err
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
	owner, p, c, err := m.openNative(ctx, context.Background(), progress)
	if err != nil {
		return nil, err
	}
	leaseCtx, leaseCancel := context.WithCancel(m.ctx)
	lease := &ClientLease{
		manager: m, owner: owner, process: p, client: c,
		ctx: leaseCtx, cancel: leaseCancel,
		gate: make(chan struct{}, 1),
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

// WithFreshClient requests a new authenticated connection before an explicit
// reconciliation callback. The persistent store owner stays held throughout.
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
	return lease.withClientContext(ctx, true, func(_ context.Context, c *client.Client) error { return fn(c) })
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

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(m.ctx, cancel)
	defer stop()
	progress := batchProgress(ctx)
	defer progress(StageDone)
	progress(StageQueue)
	if err := waitGate(ctx, m.gate, m.options.Timeout); err != nil {
		return err
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
	ctx, cancelOperation := context.WithTimeout(ctx, m.options.Timeout)
	defer cancelOperation()
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

func (m *Manager) openNative(ctx, processContext context.Context, progress func(Stage)) (owner *runtimeowner.Lock, p *process, c *client.Client, err error) {
	binary, err := resolveBinary(m.options.Binary, os.Executable, exec.LookPath)
	if err != nil {
		return nil, nil, nil, &batchError{stage: "binary resolution", cause: err}
	}
	progress(StageOwnership)
	ownerCtx, cancelOwner := context.WithTimeout(ctx, m.options.Timeout)
	owner, err = m.acquire(ownerCtx)
	cancelOwner()
	if err != nil {
		return nil, nil, nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, owner.Close())
		}
	}()
	if !m.options.ExistingOnly {
		progress(StageInitialize)
		initCtx, cancelInit := context.WithTimeout(ctx, m.options.Timeout)
		err := m.initialize(initCtx, binary)
		cancelInit()
		if err != nil {
			return owner, nil, nil, err
		}
	}
	p, c, err = m.connectNative(ctx, processContext, binary, progress)
	return owner, p, c, err
}

// connectNative assumes the caller already owns the store. Reconnecting never
// initializes a directory or releases ownership to another GSEA runtime.
func (m *Manager) connectNative(ctx, processContext context.Context, binary string, progress func(Stage)) (*process, *client.Client, error) {
	// ate serve replays the store before it reads the first protocol frame.
	// An ordinary RPC deadline here makes a sufficiently large store unopenable.
	ctx, cancel := context.WithTimeout(ctx, m.options.StartupTimeout)
	defer cancel()
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return nil, nil, &batchError{stage: "credential generation", cause: err}
	}
	token := hex.EncodeToString(secret[:])
	progress(StageStart)
	p, err := m.start(processContext, binary, token)
	if err != nil {
		return nil, nil, err
	}
	progress(StageHandshake)
	c, err := client.New(ctx, p.stream, client.Options{Token: token})
	if err != nil {
		switch {
		case ctx.Err() != nil:
			err = fmt.Errorf("ATE startup (store replay and handshake): %w", ctx.Err())
		case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
			err = &batchError{stage: "startup (connection closed before hello)", cause: err}
		default:
			err = &batchError{stage: "hello", cause: err}
		}
	} else if err = validateIdentity(c); err == nil {
		return p, c, nil
	} else {
		_ = c.Close()
	}
	_ = p.cmd.Process.Kill()
	_ = p.finish()
	p.stop()
	return nil, nil, err
}

func waitGate(ctx context.Context, gate <-chan struct{}, timeout time.Duration) error {
	wait, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case <-wait.Done():
		return wait.Err()
	case <-gate:
		return nil
	}
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
	var lastOwnershipError error
	for {
		if err := ctx.Err(); err != nil {
			if lastOwnershipError != nil && errors.Is(err, context.DeadlineExceeded) {
				return nil, errors.Join(lastOwnershipError, err)
			}
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
		if m.options.NoWaitForOwner {
			return nil, err
		}
		lastOwnershipError = err
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
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

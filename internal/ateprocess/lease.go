package ateprocess

import (
	"context"
	"errors"

	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/go-self-evolving-agent/internal/observation"
)

// WithClientContext supplies the bounded operation context. A broken SDK
// connection can be replaced before a new callback starts. Once invoked, its
// callback always returns its own outcome and is never replayed by this layer.
func (l *ClientLease) WithClientContext(ctx context.Context, fn func(context.Context, *client.Client) error) error {
	return l.withClientContext(ctx, false, fn)
}

func (l *ClientLease) withClientContext(ctx context.Context, fresh bool, fn func(context.Context, *client.Client) error) (err error) {
	if l == nil || ctx == nil || fn == nil {
		return errors.New("ATE lease requires a context and callback")
	}
	var acquired func()
	if observation.Enabled(ctx) {
		var finish func(error)
		acquired, finish = observeLease(ctx)
		defer func() {
			if v := recover(); v != nil {
				finish(errors.New("ATE callback panicked"))
				panic(v)
			}
			finish(err)
		}()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(l.ctx, cancel)
	defer stop()
	if err := waitGate(ctx, l.gate, l.manager.options.Timeout); err != nil {
		if l.ctx.Err() != nil {
			return ErrLeaseClosed
		}
		return err
	}
	defer func() { l.gate <- struct{}{} }()
	if l.ctx.Err() != nil {
		return ErrLeaseClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if acquired != nil {
		acquired()
	}
	if fresh || l.client.Closed() || l.process.exited() {
		// Save the resolved executable before retiring the process. Failed
		// handshakes leave the old (closed) pair available for another attempt.
		binary := l.process.cmd.Path
		progress := batchProgress(ctx)
		defer progress(StageDone)
		progress(StageReconnect)
		// An unsuccessful exit of the old child does not prevent a new
		// handshake. finish has already joined it before returning its status.
		_ = l.retireConnection()
		p, c, err := l.manager.connectNative(ctx, context.Background(), binary, progress)
		if err != nil {
			return err
		}
		l.process, l.client = p, c
	}
	if l.ctx.Err() != nil {
		return ErrLeaseClosed
	}
	opCtx, cancelOperation := context.WithTimeout(ctx, l.manager.options.Timeout)
	defer cancelOperation()
	if err := opCtx.Err(); err != nil {
		return err
	}
	// Some legacy callbacks use their caller's context for SDK operations.
	// Interrupt their I/O at the lease deadline, and join the close before
	// releasing the gate so it can never affect the next caller.
	c := l.client
	closed := make(chan struct{})
	stopClose := context.AfterFunc(opCtx, func() { _ = c.Close(); close(closed) })
	defer func() {
		if !stopClose() {
			<-closed
		}
	}()
	err = fn(opCtx, c)
	var unknown *client.OutcomeUnknownError
	if errors.Is(err, client.ErrClosed) || errors.As(err, &unknown) {
		_ = c.Close()
	}
	if cause := opCtx.Err(); cause != nil {
		return errors.Join(cause, err)
	}
	return err
}

func (l *ClientLease) WithClient(ctx context.Context, fn func(*client.Client) error) error {
	if fn == nil {
		return errors.New("ATE lease requires a callback")
	}
	return l.WithClientContext(ctx, func(_ context.Context, c *client.Client) error { return fn(c) })
}

func (p *process) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// The gate is held while retiring or publishing a connection.
func (l *ClientLease) retireConnection() error {
	_ = l.client.Close()
	err := l.process.finish()
	l.process.stop()
	if err != nil {
		return &batchError{stage: "subprocess shutdown", cause: err}
	}
	return nil
}

// Close cancels the client lease, waits for the active operation and releases process
// ownership once.
func (l *ClientLease) Close() error {
	if l == nil {
		return nil
	}
	l.closeOnce.Do(func() {
		l.cancel()
		<-l.gate
		defer func() { l.gate <- struct{}{} }()
		l.closeErr = errors.Join(l.retireConnection(), l.owner.Close())
		l.manager.mu.Lock()
		l.manager.lease = nil
		l.manager.mu.Unlock()
		l.manager.gate <- struct{}{}
		l.manager.active.Done()
	})
	return l.closeErr
}

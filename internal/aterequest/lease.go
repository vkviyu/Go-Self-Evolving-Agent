package aterequest

import (
	"context"
	"errors"
	"sync"

	"github.com/vkviyu/agent-temporal-engine/client"
)

var ErrLeaseClosed = errors.New("ATE client lease has ended or already started")

type reconciliationDeferredKey struct{}

// DeferReconciliation marks a bounded storage scope in which an ambiguous
// request must remain pending until the enclosing callback has unwound. A
// fresh owner connection cannot be opened while that callback still holds the
// current persistent lease.
func DeferReconciliation(ctx context.Context) context.Context {
	if ctx == nil {
		return nil
	}
	return context.WithValue(ctx, reconciliationDeferredKey{}, true)
}

func ReconciliationDeferred(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	deferred, _ := ctx.Value(reconciliationDeferredKey{}).(bool)
	return deferred
}

// WithClient joins a started callback even when the process returns early.
// Late or repeated callbacks cannot use the client. The callback must use the
// supplied context and cooperate with cancellation. Completed callback outcomes
// take precedence over process shutdown errors, particularly durable receipts.
func WithClient(ctx context.Context, process interface {
	WithClient(context.Context, func(*client.Client) error) error
}, fn func(context.Context, *client.Client) error) error {
	if ctx == nil || process == nil || fn == nil {
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	var started, returned bool
	var result error
	done := make(chan struct{})
	processErr := process.WithClient(ctx, func(c *client.Client) (err error) {
		mu.Lock()
		if returned || started {
			mu.Unlock()
			return ErrLeaseClosed
		}
		started = true
		mu.Unlock()
		defer func() {
			result = err
			close(done)
		}()
		return fn(ctx, c)
	})
	mu.Lock()
	returned = true
	join := started
	mu.Unlock()
	if !join {
		return errors.Join(ErrLeaseClosed, processErr)
	}
	select {
	case <-done:
		return result
	default:
		cancel()
		<-done
		if result == nil {
			// The callback may have received a durable acknowledgement just
			// before the process returned. Joining makes that result observable.
			return nil
		}
		return errors.Join(processErr, result)
	}
}

// Package approval bridges policy decisions to interactive frontends. The
// broker has no terminal dependency: a UI receives requests and resolves them
// while the Agent execution goroutine waits on the supplied context.
package approval

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
)

var (
	ErrUnknownRequest = errors.New("approval request is no longer pending")
	ErrBrokerClosed   = errors.New("approval broker is closed")
)

type (
	// Request is a UI-safe snapshot of the policy request and its decision.
	Request struct {
		ID       string
		Policy   policy.Request
		Decision policy.Decision
		// Source names the delegated Agent behind the request. It is zero for
		// the root Agent.
		Source  Source
		Created time.Time
	}

	// Source describes the delegated Agent that asked for approval.
	Source struct {
		AgentID orchestration.AgentID
		Profile string
		Task    string
	}

	// SourceDescriber resolves the delegated Agent behind an actor scope. It
	// returns a zero Source for the root Agent.
	SourceDescriber func(context.Context, orchestration.ActorScope) Source

	// BrokerOption configures a Broker at construction.
	BrokerOption func(*Broker)

	// Broker coordinates one or more approval requests for an interactive run.
	Broker struct {
		requests chan Request
		done     chan struct{}
		describe SourceDescriber
		mu       sync.Mutex
		// pending holds the result channel of each unresolved request.
		pending map[string]chan bool
		closed  bool
		nextID  atomic.Uint64
	}

	// DenyApprover makes non-interactive execution fail closed without blocking.
	DenyApprover struct{}
)

// NewBroker creates a broker with a bounded request queue.
func NewBroker(buffer int, options ...BrokerOption) *Broker {
	if buffer <= 0 {
		buffer = 16
	}
	broker := &Broker{requests: make(chan Request, buffer), done: make(chan struct{}), pending: make(map[string]chan bool)}
	for _, option := range options {
		option(broker)
	}
	return broker
}

// WithSourceDescriber labels each request with the delegated Agent that made
// it, so a frontend can show who is asking while children run in parallel.
func WithSourceDescriber(describe SourceDescriber) BrokerOption {
	return func(broker *Broker) {
		broker.describe = describe
	}
}

// Requests exposes pending requests to a UI without exposing internal state.
func (broker *Broker) Requests() <-chan Request {
	if broker == nil {
		return nil
	}
	return broker.requests
}

// Done closes when the broker is no longer able to accept approvals.
func (broker *Broker) Done() <-chan struct{} {
	if broker == nil {
		return nil
	}
	return broker.done
}

// Approve implements policy.Approver and blocks until the UI resolves request.
// Request IDs are generated rather than taken from the tool call ID: parallel
// Agents may reuse call IDs, and a shared key would resolve the wrong waiter.
func (broker *Broker) Approve(ctx context.Context, request policy.Request, decision policy.Decision) (bool, error) {
	if broker == nil {
		return false, ErrBrokerClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	id := "approval-" + strconv.FormatUint(broker.nextID.Add(1), 36)
	item := Request{ID: id, Policy: clonePolicyRequest(request), Decision: decision, Created: time.Now().UTC()}
	if broker.describe != nil {
		item.Source = broker.describe(ctx, request.Actor)
	}
	result := make(chan bool, 1)
	broker.mu.Lock()
	if broker.closed {
		broker.mu.Unlock()
		return false, ErrBrokerClosed
	}
	broker.pending[id] = result
	broker.mu.Unlock()
	defer func() {
		broker.mu.Lock()
		delete(broker.pending, id)
		broker.mu.Unlock()
	}()
	select {
	case broker.requests <- item:
	case <-broker.done:
		return false, ErrBrokerClosed
	case <-ctx.Done():
		return false, ctx.Err()
	}
	select {
	case approved := <-result:
		return approved, nil
	case <-broker.done:
		return false, ErrBrokerClosed
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// Resolve wakes the Agent waiting for requestID.
func (broker *Broker) Resolve(requestID string, approved bool) error {
	if broker == nil {
		return ErrBrokerClosed
	}
	broker.mu.Lock()
	result, ok := broker.pending[requestID]
	broker.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownRequest, requestID)
	}
	// The buffered channel keeps the first decision; later ones are dropped.
	select {
	case result <- approved:
	default:
	}
	return nil
}

// Close causes future approvals to fail and releases all pending waiters.
func (broker *Broker) Close() error {
	if broker == nil {
		return nil
	}
	broker.mu.Lock()
	if broker.closed {
		broker.mu.Unlock()
		return nil
	}
	broker.closed = true
	for _, result := range broker.pending {
		select {
		case result <- false:
		default:
		}
	}
	close(broker.done)
	broker.mu.Unlock()
	return nil
}

func (DenyApprover) Approve(context.Context, policy.Request, policy.Decision) (bool, error) {
	return false, nil
}

func clonePolicyRequest(request policy.Request) policy.Request {
	request.NormalizedArg = append([]byte(nil), request.NormalizedArg...)
	return request
}

var _ policy.Approver = (*Broker)(nil)
var _ policy.Approver = DenyApprover{}

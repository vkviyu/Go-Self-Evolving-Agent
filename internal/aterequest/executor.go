package aterequest

import (
	"context"
	"errors"
	"fmt"

	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/agent-temporal-engine/protocol"
)

var (
	ErrUnknown          = errors.New("ATE commit outcome unknown")
	ErrRejected         = errors.New("ATE command rejected")
	ErrCleanup          = errors.New("ATE committed; pending payload cleanup failed")
	ErrRejectionCleanup = errors.New("ATE rejected; terminal rejection cleanup failed")
)

type (
	// Client is the native ATE write/reconciliation surface.
	Client interface {
		Commit(context.Context, api.CommitRequest) (api.CommitResult, error)
		LookupRequest(context.Context, api.LookupRequestRequest) (api.LookupRequestResult, error)
	}

	Executor struct {
		client  Client
		pending PendingStore
		storeID model.StoreID
	}

	// CleanupError is known committed, never an unknown or rejected outcome.
	// Retry Resume with Key to verify the durable result and retry deletion.
	CleanupError struct {
		Key    api.RequestKey
		Result api.CommitResult
		Cause  error
	}

	// RejectionCleanupError is conclusively rejected, not committed or unknown.
	// BeginAttempt already prevents replay, even if recording the rejection
	// itself failed. Retry only DeleteRejected with this exact response.
	RejectionCleanupError struct {
		Key    api.RequestKey
		Result api.CommitResult
		Cause  error
	}
)

func (err *CleanupError) Error() string        { return fmt.Sprintf("%s: %v", ErrCleanup, err.Cause) }
func (err *CleanupError) Unwrap() error        { return err.Cause }
func (err *CleanupError) Is(target error) bool { return target == ErrCleanup }

func (err *RejectionCleanupError) Error() string {
	return fmt.Sprintf("%s: %v", ErrRejectionCleanup, err.Cause)
}
func (err *RejectionCleanupError) Unwrap() error { return err.Cause }
func (err *RejectionCleanupError) Is(target error) bool {
	return target == ErrRejectionCleanup || target == ErrRejected
}

// NewExecutor binds the ATE client and pending command store to a specific authority store
// ID.
func NewExecutor(client Client, pending PendingStore, storeID model.StoreID) (*Executor, error) {
	if client == nil || pending == nil || !validStoreID(storeID) {
		return nil, fmt.Errorf("%w: client, pending store and ATE store identity are required", ErrInvalid)
	}
	return &Executor{client: client, pending: pending, storeID: storeID}, nil
}

// Execute persists the caller-supplied stable identity and original bytes before
// network access. It never generates identities, including on unknown outcomes.
func (executor *Executor) Execute(ctx context.Context, request api.CommitRequest) (api.CommitResult, error) {
	record, err := NewRecord(executor.storeID, request)
	if err != nil {
		return api.CommitResult{}, err
	}
	if err := executor.pending.Insert(ctx, record); err != nil {
		// A rejection marker can be durable while its payload deletion was
		// interrupted. Insert must fence that identity; the saved receipt still
		// permits local cleanup of the identical envelope, never another send.
		if errors.Is(err, ErrRejected) {
			stored, loadErr := executor.load(ctx, request.Key())
			if loadErr == nil && Equal(stored, record) && stored.Rejection != nil {
				return executor.reject(ctx, stored, *stored.Rejection)
			}
			if loadErr == nil {
				return api.CommitResult{}, ErrConflict
			}
			if !errors.Is(loadErr, ErrNotFound) {
				return api.CommitResult{}, errors.Join(err, loadErr)
			}
		}
		return api.CommitResult{}, err
	}
	stored, err := executor.load(ctx, request.Key())
	if err != nil {
		return api.CommitResult{}, err
	}
	if !Equal(stored, record) {
		return api.CommitResult{}, ErrConflict
	}
	return executor.run(ctx, stored)
}

// Resume submits an unattempted saved envelope or reconciles a prior attempt.
// ATE does not persist rejections, so durable absence after an attempt cannot
// distinguish a lost submission from a lost rejection. It never authorizes
// automatic resubmission. A missing row is not proof of a durable ack.
func (executor *Executor) Resume(ctx context.Context, key api.RequestKey) (api.CommitResult, error) {
	record, err := executor.load(ctx, key)
	if err != nil {
		return api.CommitResult{}, err
	}
	return executor.run(ctx, record)
}

// ReconcileAcknowledged never submits a command or acquires an attempt. Startup
// may clean a verified durable ACK or a locally saved rejection; durable absence
// leaves the original record and attempt state untouched. CodeNotFoundAtFence
// reports only that observation, not a resolved outcome for an attempted write.
func (executor *Executor) ReconcileAcknowledged(ctx context.Context, key api.RequestKey) (api.CommitResult, error) {
	record, err := executor.load(ctx, key)
	if err != nil {
		return api.CommitResult{}, err
	}
	if record.Rejection != nil {
		return executor.reject(ctx, record, *record.Rejection)
	}
	lookup, err := executor.lookup(ctx, record)
	if err != nil {
		return unknown(record, err)
	}
	switch lookup.Code {
	case api.CodeDurableAck:
		return executor.finish(ctx, record, lookup.Result)
	case api.CodeNotFoundAtFence:
		return api.CommitResult{Code: api.CodeNotFoundAtFence, RequestID: key.RequestID}, nil
	default:
		return unknown(record, fmt.Errorf("lookup returned %q", lookup.Code))
	}
}

func (executor *Executor) load(ctx context.Context, key api.RequestKey) (Record, error) {
	if err := key.Validate(); err != nil {
		return Record{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	record, found, err := executor.pending.Get(ctx, key)
	if err != nil {
		return Record{}, err
	}
	if !found {
		return Record{}, ErrNotFound
	}
	if err := record.Validate(); err != nil {
		return Record{}, err
	}
	if record.Request.Key() != key || record.StoreID != executor.storeID {
		return Record{}, ErrConflict
	}
	// Isolate the network call from memory owned by a store implementation.
	cloned, err := NewRecord(record.StoreID, record.Request)
	cloned.Attempted = record.Attempted
	if record.Rejection != nil {
		result := *record.Rejection
		cloned.Rejection = &result
	}
	return cloned, err
}

func (executor *Executor) run(ctx context.Context, record Record) (api.CommitResult, error) {
	if record.Rejection != nil {
		return executor.reject(ctx, record, *record.Rejection)
	}
	lookup, err := executor.lookup(ctx, record)
	if err != nil {
		return unknown(record, err)
	}
	if lookup.Code == api.CodeDurableAck {
		return executor.finish(ctx, record, lookup.Result)
	}
	if lookup.Code != api.CodeNotFoundAtFence {
		return unknown(record, fmt.Errorf("lookup returned %q", lookup.Code))
	}
	if record.Attempted {
		return unknown(record, errors.New("attempt already started; absence cannot prove it was never rejected"))
	}
	// Fence the attempt durably before network access. A cleanup failure or a
	// crash after rejection must not make the same bytes eligible to run later.
	started, err := executor.pending.BeginAttempt(ctx, record)
	if err != nil || !started {
		return unknown(record, errors.Join(err, errors.New("attempt was not acquired")))
	}
	request, err := NewRecord(record.StoreID, record.Request)
	if err != nil {
		return unknown(record, err)
	}
	result, commitErr := executor.client.Commit(ctx, request.Request)
	if result.Code == api.CodeDurableAck && commitErr == nil {
		return executor.finish(ctx, record, result)
	}
	// Error-bearing native rejections require a conclusive semantic error
	// category. Partial decode and transport errors never authorize deletion.
	if (result.Code == api.CodeRejected || result.Code == api.CodeConflict) &&
		ctx.Err() == nil && executor.conclusiveRejectionError(result, commitErr) {
		if err := VerifyRejection(record, result); err != nil {
			return unknown(record, err)
		}
		rejected, err := executor.reject(ctx, record, result)
		return rejected, errors.Join(err, commitErr)
	}
	// A lost response may already be durable. Reconcile once; later calls use
	// the same key and bytes. Never spin or turn cancellation into rejection.
	if err := ctx.Err(); err != nil {
		// The caller must release this lease before opening a fresh bounded
		// reconciliation batch. An expired context cannot obtain a receipt.
		return unknown(record, errors.Join(commitErr, err,
			errors.New("submission context ended; reconcile original request in a fresh storage batch")))
	}
	lookup, lookupErr := executor.lookup(ctx, record)
	if lookupErr == nil && lookup.Code == api.CodeDurableAck {
		return executor.finish(ctx, record, lookup.Result)
	}
	return unknown(record, errors.Join(ctx.Err(), commitErr, lookupErr, fmt.Errorf("commit returned %q; lookup returned %q", result.Code, lookup.Code)))
}

func (executor *Executor) conclusiveRejectionError(result api.CommitResult, err error) bool {
	if err == nil {
		return true
	}
	if result.Code != api.CodeRejected || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var class api.ErrorClass
	// Native socket clients return protocol.Error only after a complete,
	// correlated response. Do not unwrap a transport/unknown error to find one.
	if remote, ok := err.(*protocol.Error); ok {
		class = remote.Code
	} else if classifier, ok := executor.client.(interface{ ClassifyError(error) api.ErrorClass }); ok {
		class = classifier.ClassifyError(err)
	}
	switch class {
	case api.ErrorInvalidRequest, api.ErrorUnauthorized, api.ErrorUnsupported, api.ErrorConflict:
		return true
	default:
		return false
	}
}

func (executor *Executor) reject(ctx context.Context, record Record, result api.CommitResult) (api.CommitResult, error) {
	if err := VerifyRejection(record, result); err != nil {
		return unknown(record, err)
	}
	if err := executor.pending.DeleteRejected(ctx, record, result); err != nil {
		return result, &RejectionCleanupError{Key: record.Request.Key(), Result: result, Cause: err}
	}
	return result, ErrRejected
}

func (executor *Executor) lookup(ctx context.Context, record Record) (api.LookupRequestResult, error) {
	key := record.Request.Key()
	result, err := executor.client.LookupRequest(ctx, api.LookupRequestRequest{
		Space: key.Space, Principal: key.Principal, RequestID: key.RequestID,
	})
	if err != nil {
		return result, err
	}
	if result.Code == api.CodeDurableAck || result.Code == api.CodeNotFoundAtFence {
		if result.ObservedFence.Store != record.StoreID || result.ObservedFence.Validate() != nil {
			return result, fmt.Errorf("%w: lookup observed another or invalid store fence", ErrConflict)
		}
		if result.Code == api.CodeDurableAck {
			if err := VerifyAcknowledgement(record, result.Result); err != nil {
				return result, err
			}
			if result.ObservedFence.LSN < result.Result.Fence.LSN ||
				(result.ObservedFence.LSN == result.Result.Fence.LSN && result.ObservedFence != result.Result.Fence) {
				return result, fmt.Errorf("%w: lookup fence predates or contradicts acknowledgement", ErrConflict)
			}
		}
	}
	return result, nil
}

func (executor *Executor) finish(ctx context.Context, record Record, result api.CommitResult) (api.CommitResult, error) {
	if err := VerifyAcknowledgement(record, result); err != nil {
		return unknown(record, err)
	}
	if err := executor.pending.DeleteAcknowledged(ctx, record, result); err != nil {
		return result, &CleanupError{Key: record.Request.Key(), Result: result, Cause: err}
	}
	return result, nil
}

// unknown returns the request ID with CodeCommitUnknown and preserves the underlying cause
// for reconciliation.
func unknown(record Record, cause error) (api.CommitResult, error) {
	return api.CommitResult{Code: api.CodeCommitUnknown, RequestID: record.Request.RequestID}, errors.Join(ErrUnknown, cause)
}

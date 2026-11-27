// Package ateauthority provides transactions over individual JSON records in
// an ATE authority space. ATE is the only business-state authority; Pending
// stores only aterequest's immutable in-flight command envelopes.
package ateauthority

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/agent-temporal-engine/protocol"
	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
)

// MaxUpdateAttempts includes the initial attempt. Only a conclusively rejected
// ExpectedRoot precondition permits another invocation of an Update callback.
const MaxUpdateAttempts = 4

var (
	ErrInvalid  = errors.New("invalid authority transaction argument")
	ErrReadOnly = errors.New("authority transaction is read-only")
	ErrClosed   = aterequest.ErrLeaseClosed
	ErrExists   = errors.New("authority record already exists")
	ErrConflict = errors.New("authority root conflict")
	ErrLimit    = errors.New("authority transaction limit exceeded")
	ErrCorrupt  = errors.New("invalid authority JSON record")
	ErrIdentity = errors.New("authority identity or anchor mismatch")
)

// Process supplies a connection for one bounded callback, at most once, without
// transparently replaying it. *ateprocess.Manager implements this interface.
type Process interface {
	WithClient(context.Context, func(*client.Client) error) error
}

type Options struct {
	Process   Process
	Pending   aterequest.PendingStore
	Space     model.SpaceID
	Principal model.PrincipalID
}

// Store retains configuration only, never a business-state projection or client.
// Each callback must use its Tx on one goroutine, must not retain it, and must
// avoid external side effects. Update may invoke its callback up to four times.
// View/Update/Resume join any started process callback before returning, even
// when Process returns early. Callbacks must cooperate with cancellation;
// arbitrary Go code cannot be forcibly stopped to enforce a return deadline.
type Store struct{ options Options }

// RequestError exposes recovery identity and native outcome without losing
// errors.Is/As classification. ErrUnknown and ErrCleanup come from aterequest;
// neither permits rerunning the business callback. Use Resume with Key instead.
// Pending.List can discover envelopes if the process itself interrupted before
// it could return this error. The executor retires proven rejections through
// PendingStore's terminal identity fence before any fresh callback attempt.
type RequestError struct {
	Key     api.RequestKey
	StoreID model.StoreID
	Result  api.CommitResult
	Cause   error
}

func (e *RequestError) Error() string {
	return fmt.Sprintf("authority request %s: %v", e.Key.RequestID, e.Cause)
}
func (e *RequestError) Unwrap() error { return e.Cause }

// New validates configuration without opening ATE or writing local files.
func New(options Options) (*Store, error) {
	key := model.RequestKey{Space: options.Space, Principal: options.Principal, RequestID: "validate"}
	if nilValue(options.Process) || nilValue(options.Pending) || key.Validate() != nil || strings.HasPrefix(string(options.Space), "ate:") {
		return nil, ErrInvalid
	}
	return &Store{options: options}, nil
}

func nilValue(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Func, reflect.Map, reflect.Slice, reflect.Chan:
		return v.IsNil()
	}
	return false
}

func (s *Store) withClient(ctx context.Context, fn func(context.Context, *client.Client) error) error {
	return aterequest.WithClient(ctx, s.options.Process, fn)
}

// reconcilePending clears records whose outcome ATE can still prove. An
// attempted request is never resubmitted; Resume only reads its receipt and
// deletes the envelope after a durable ACK. An unattempted record cannot be
// safely attributed to this caller, so it still blocks new work.
func (s *Store) reconcilePending(ctx context.Context, c *client.Client, store model.StoreID) error {
	records, err := aterequest.ListPending(ctx, s.options.Pending)
	if err != nil {
		return err
	}
	for _, record := range records {
		key := record.Request.Key()
		if key.Space != s.options.Space || key.Principal != s.options.Principal {
			continue
		}
		if record.StoreID != store {
			return requestError(key, record.StoreID, api.CommitResult{}, ErrIdentity)
		}
		if record.Rejection != nil {
			return requestError(key, store, *record.Rejection, &aterequest.RejectionCleanupError{
				Key: key, Result: *record.Rejection, Cause: errors.New("saved rejection awaits Resume cleanup"),
			})
		}
		if !record.Attempted {
			return requestError(key, store, api.CommitResult{Code: api.CodeCommitUnknown, RequestID: key.RequestID}, aterequest.ErrUnknown)
		}
		executor, err := aterequest.NewExecutor(c, s.options.Pending, store)
		if err != nil {
			return err
		}
		if _, err = executor.Resume(ctx, key); err != nil {
			return requestError(key, store, api.CommitResult{Code: api.CodeCommitUnknown, RequestID: key.RequestID}, err)
		}
	}
	return nil
}

// View runs fn once at a fresh immutable ATE anchor. Mutations return ErrReadOnly.
func (s *Store) View(ctx context.Context, fn func(*Tx) error) error {
	return s.run(ctx, fn, false)
}

// Update commits all staged records atomically with a CAS over the entire space
// root, protecting reads of absent keys and List phantoms as well as writes.
// A callback error aborts without submitting. An empty write set needs no commit.
// Before invoking fn (including retries), Update checks for pending requests in
// this store/space/principal. Any pending request blocks fn with its original
// RequestError key. Resolve it explicitly with Resume before fresh business work.
func (s *Store) Update(ctx context.Context, fn func(*Tx) error) error {
	return s.run(ctx, fn, true)
}

func (s *Store) run(ctx context.Context, fn func(*Tx) error, writable bool) error {
	if ctx == nil || fn == nil {
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.withClient(ctx, func(ctx context.Context, c *client.Client) error {
		for attempt := 0; ; attempt++ {
			tx, err := s.begin(ctx, c, writable)
			// A concurrent in-process publication briefly fences reads. It is
			// transient, unlike a durable authorization change, so a bounded
			// retry keeps failure recording and close-out possible.
			for retry := 0; err != nil && errors.Is(err, api.ErrReadAuthorizationChanged) && retry < 20 && ctx.Err() == nil; retry++ {
				time.Sleep(5 * time.Millisecond)
				tx, err = s.begin(ctx, c, writable)
			}
			if err != nil {
				return err
			}
			if writable {
				if err := s.reconcilePending(ctx, c, tx.view.Anchor.Fence.Store); err != nil {
					return errors.Join(err, tx.close())
				}
			}
			// End the callback and release the view before submitting. A close
			// failure is thus never reported as an ambiguous business commit.
			err = func() (err error) {
				defer func() { err = errors.Join(err, tx.close()) }()
				return fn(tx)
			}()
			err = errors.Join(err, ctx.Err())
			if err != nil || !writable || len(tx.writes) == 0 {
				return err
			}
			request, err := tx.request(s.options)
			if err != nil {
				return err
			}
			executor, err := aterequest.NewExecutor(c, s.options.Pending, tx.view.Anchor.Fence.Store)
			if err != nil {
				return err
			}
			result, err := executor.Execute(ctx, request)
			if err == nil {
				return nil
			}
			provenConflict := errors.Is(err, aterequest.ErrRejected) &&
				!errors.Is(err, aterequest.ErrRejectionCleanup) &&
				result.Code == api.CodeConflict && result.Conflict == api.ConflictPrecondition
			if provenConflict && attempt+1 < MaxUpdateAttempts && ctx.Err() == nil {
				continue
			}
			return requestError(request.Key(), tx.view.Anchor.Fence.Store, result, err)
		}
	})
}

// requestError translates native commit conflicts into authority errors while preserving
// the request identity for reconciliation.
func requestError(key api.RequestKey, store model.StoreID, result api.CommitResult, err error) error {
	if errors.Is(err, aterequest.ErrRejected) && result.Code == api.CodeConflict {
		switch result.Conflict {
		case api.ConflictPrecondition:
			err = errors.Join(ErrConflict, err)
		case api.ConflictIdempotency:
			err = errors.Join(aterequest.ErrConflict, err)
		}
	}
	return &RequestError{Key: key, StoreID: store, Result: result, Cause: err}
}

// Resume reconciles only the saved native envelope, never a callback.
// It also retries cleanup after a known durable commit. The pending record must
// belong to this space, principal, native store, and core.state.mutate/v1.
// Only an unattempted envelope can be submitted. Absence after an attempted
// request remains unknown, as native ATE does not persist rejection receipts.
func (s *Store) Resume(ctx context.Context, key api.RequestKey) error {
	if ctx == nil || key.Validate() != nil || key.Space != s.options.Space || key.Principal != s.options.Principal {
		return ErrInvalid
	}
	return s.withClient(ctx, func(ctx context.Context, c *client.Client) error {
		tx, err := s.begin(ctx, c, false)
		if err != nil {
			return err
		}
		if err := tx.close(); err != nil {
			return err
		}
		record, found, err := s.options.Pending.Get(ctx, key)
		if err != nil {
			return err
		}
		if !found {
			return aterequest.ErrNotFound
		}
		if err := record.Validate(); err != nil {
			return err
		}
		if record.Request.Key() != key || record.StoreID != tx.view.Anchor.Fence.Store ||
			record.Request.Method != api.MethodMutateState {
			return ErrIdentity
		}
		executor, err := aterequest.NewExecutor(c, s.options.Pending, record.StoreID)
		if err != nil {
			return err
		}
		result, err := executor.Resume(ctx, key)
		if err != nil {
			return requestError(key, record.StoreID, result, err)
		}
		return nil
	})
}

func (s *Store) begin(ctx context.Context, c *client.Client, writable bool) (*Tx, error) {
	if c == nil || c.Identity().Principal != s.options.Principal {
		return nil, ErrIdentity
	}
	view, err := c.OpenView(ctx, api.OpenViewRequest{Space: s.options.Space, Principal: s.options.Principal, Mode: "state"})
	if err != nil {
		return nil, mapReadError(err)
	}
	tx := &Tx{ctx: ctx, client: c, view: view, writable: writable, active: true, writes: make(map[string]api.StateMutation)}
	if view.Anchor.Validate() != nil || view.Anchor.Space != s.options.Space || view.Anchor.Cursor != nil || view.Anchor.Knowledge != nil {
		return nil, errors.Join(ErrIdentity, tx.close())
	}
	// OpenView provides the native store identity but not the domain state
	// root. A bounded anchored point read supplies that root even if empty.
	result, err := c.GetViewState(ctx, api.ReadViewStateRequest{View: view.Ref})
	if err != nil {
		return nil, errors.Join(mapReadError(err), tx.close())
	}
	tx.root, tx.rootSet = result.StateRoot, result.RootSet
	if result.Fence != view.Anchor.Fence {
		return nil, errors.Join(ErrIdentity, tx.close())
	}
	return tx, nil
}

// close invalidates the transaction and gives the view-close request its own short
// deadline.
func (tx *Tx) close() error {
	tx.active = false
	ctx, cancel := context.WithTimeout(context.WithoutCancel(tx.ctx), time.Second)
	defer cancel()
	return mapReadError(tx.client.CloseView(ctx, api.CloseViewRequest{Ref: tx.view.Ref}))
}

func (tx *Tx) request(options Options) (api.CommitRequest, error) {
	ops := make([]api.StateMutation, 0, len(tx.writes))
	for _, op := range tx.writes {
		ops = append(ops, op)
	}
	body, err := api.EncodeStateBatch(api.StateBatch{ExpectedRoot: &tx.root, Operations: ops})
	if err != nil {
		return api.CommitRequest{}, err
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return api.CommitRequest{}, err
	}
	request := api.CommitRequest{Space: options.Space, Principal: options.Principal,
		RequestID: model.RequestID(hex.EncodeToString(id[:])), Method: api.MethodMutateState, Body: body}
	if tx.fenceAll {
		fence := tx.view.Anchor.Fence
		request.ExpectedFence = &fence
	}
	return request, nil
}

// mapReadError makes a native resource-limit failure recognizable as ErrLimit while
// preserving the remote error.
func mapReadError(err error) error {
	var remote *protocol.Error
	if errors.As(err, &remote) && remote.Code == api.ErrorResourceLimit {
		return errors.Join(ErrLimit, err)
	}
	return err
}

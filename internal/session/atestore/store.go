// Package atestore stores session history solely in native ATE Session and
// SessionEvent records. PendingStore contains only unresolved command envelopes,
// never a committed-history mirror.
//
// Large envelopes are staged as native payload chunks and published through a
// single atomic Session append. No event is truncated or mirrored locally.
// App, CLI, pending-store persistence and checkpoint wiring are caller-owned.
// ATE has no durable rejection receipts. After an attempted request, absence
// remains unknown: aterequest retains its exact envelope and blocks automatic
// resubmission. Definitively received rejections are retired by PendingStore.
// Resolving ambiguous absence requires a future native receipt/attempt protocol.
//
// Reads default to 1000 events, cap at 10000 and reject aggregate envelopes
// above 8 MiB. Pending admission requires a complete scan of at most 128
// requests within 32 MiB. Process must serialize leases across writers sharing
// this ATE store (ateprocess.Manager does); Pending must survive host restarts.
package atestore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateports"
	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

var (
	ErrEventTooLarge    = errors.New("atestore: event exceeds native payload limit")
	ErrPayloadReference = errors.New("atestore: payload reference requires verified readback")
	ErrLimit            = errors.New("atestore: operation exceeds bounded capacity")
	ErrCorrupt          = errors.New("atestore: corrupt or incomplete native history")
	ErrIdentity         = errors.New("atestore: unexpected ATE authority")
	ErrPending          = errors.New("atestore: a different append is awaiting reconciliation")
)

// MaxReadBytes bounds complete original envelope bytes per public read call.
const MaxReadBytes = 8 << 20

// MaxAppendBytes leaves room in the native 4 MiB state batch for chunk keys,
// framing and the recovery descriptor. It bounds complete original envelopes.
const MaxAppendBytes = 3 << 20

// Process is implemented by ateprocess.Manager. Clients must not outlive fn.
type Process interface {
	WithClient(context.Context, func(*client.Client) error) error
}

type freshProcess interface {
	WithFreshClient(context.Context, func(*client.Client) error) error
}

// Executor deliberately requires only the existing aterequest write boundary.
// Implementations must persist exact requests before submission and reconcile
// unknown outcomes by their original key, digest and ATE store identity.
type Executor interface {
	Execute(context.Context, api.CommitRequest) (api.CommitResult, error)
}

type ExecutorFactory func(*client.Client, model.StoreID) (Executor, error)

type Options struct {
	Process Process
	Pending aterequest.PendingStore
	// Space and StoreID are explicit stable authority identities, never derived
	// from a SQLite filename. The space must already exist in ATE.
	Space   model.SpaceID
	StoreID model.StoreID
	// NewExecutor defaults to aterequest.NewExecutor using Pending. An override
	// must use that same PendingStore and validate durable acknowledgements.
	// Fresh reconciliation uses the native executor's Resume API directly.
	NewExecutor ExecutorFactory
	// ReconciliationTimeout opts Append into one fresh, bounded process lease
	// after an unknown outcome or deadline following durable plan preparation.
	// It retains context values but ignores the original cancellation/deadline.
	// Zero disables reconciliation. Negative values are invalid. Callers own
	// the total budget, including process timeout and shutdown margin.
	ReconciliationTimeout time.Duration
}

type Store struct {
	process               Process
	pending               aterequest.PendingStore
	space                 model.SpaceID
	storeID               model.StoreID
	newExecutor           ExecutorFactory
	gate                  chan struct{}
	reconciliationTimeout time.Duration
}

func New(o Options) (*Store, error) {
	if o.ReconciliationTimeout < 0 {
		return nil, errors.New("atestore: reconciliation timeout must not be negative")
	}
	if nilValue(o.Process) || nilValue(o.Pending) || !validIdentity(string(o.Space)) ||
		!validIdentity(string(o.StoreID)) {
		return nil, errors.New("atestore: process, pending store, explicit space and store ID are required")
	}
	factory := o.NewExecutor
	if factory == nil {
		factory = func(c *client.Client, id model.StoreID) (Executor, error) {
			return aterequest.NewExecutor(c, o.Pending, id)
		}
	}
	s := &Store{process: o.Process, pending: o.Pending, space: o.Space,
		storeID: o.StoreID, newExecutor: factory, gate: make(chan struct{}, 1),
		reconciliationTimeout: o.ReconciliationTimeout}
	s.gate <- struct{}{}
	return s, nil
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

// withClient joins callbacks before releasing caller-owned state and cancels
// their context if the process lease ends early.
func (s *Store) withClient(ctx context.Context, fn func(context.Context, *client.Client) error) error {
	if ctx == nil {
		return errors.New("atestore: context required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return aterequest.WithClient(ctx, s.process, func(ctx context.Context, c *client.Client) error {
		if c == nil || c.Identity().Principal != "gsea" {
			return ErrIdentity
		}
		snapshot, err := c.Snapshot(ctx)
		if err != nil {
			return err
		}
		if snapshot.Fence.Store != s.storeID {
			return ErrIdentity
		}
		return fn(ctx, c)
	})
}

func (s *Store) withFreshClient(ctx context.Context, fn func(context.Context, *client.Client) error) error {
	if ctx == nil {
		return errors.New("atestore: context required")
	}
	if aterequest.ReconciliationDeferred(ctx) {
		return s.withClient(ctx, fn)
	}
	if fresh, ok := s.process.(freshProcess); ok {
		return fresh.WithFreshClient(ctx, func(c *client.Client) error {
			if c == nil || c.Identity().Principal != "gsea" {
				return ErrIdentity
			}
			snapshot, err := c.Snapshot(ctx)
			if err != nil {
				return err
			}
			if snapshot.Fence.Store != s.storeID {
				return ErrIdentity
			}
			return fn(ctx, c)
		})
	}
	return s.withClient(ctx, fn)
}

func randomID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func closeView(ctx context.Context, c interface {
	CloseView(context.Context, api.CloseViewRequest) error
}, ref model.ViewRef) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	return c.CloseView(cleanup, api.CloseViewRequest{Ref: ref})
}

func pageLimit(limit int) int {
	if limit <= 0 {
		return ateports.DefaultLimit
	}
	return min(limit, ateports.MaxLimit)
}

func checkID(id session.ID) error {
	if !validIdentity(string(id)) {
		return fmt.Errorf("%w: invalid session ID", session.ErrInvalidEvent)
	}
	return nil
}

func validIdentity(value string) bool {
	return validText(value) && len(value) <= model.MaxIdentityBytes
}

var (
	_ session.EventStore           = (*Store)(nil)
	_ session.CorrelatedEventStore = (*Store)(nil)
	_ ateports.HistoryReader       = (*Store)(nil)
	_ ateports.HistoryCutStore     = (*Store)(nil)
)

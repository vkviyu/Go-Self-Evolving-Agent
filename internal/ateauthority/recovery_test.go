package ateauthority

import (
	"context"
	"errors"
	"testing"

	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
)

type processFunc func(context.Context, func(*client.Client) error) error

func (f processFunc) WithClient(ctx context.Context, fn func(*client.Client) error) error {
	return f(ctx, fn)
}

func TestPendingRequestBlocksFreshBusinessCallback(t *testing.T) {
	for _, mode := range []string{"unknown-absent", "unknown-durable", "unattempted", "durable-cleanup", "rejected-cleanup"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.backend.unknownBefore.Store(mode == "unknown-absent")
			f.backend.loseReply.Store(mode == "unknown-durable")
			f.backend.failLookup.Store(mode == "unattempted")
			f.pending.failCleanup = mode == "durable-cleanup"
			f.pending.failRejectCleanup = mode == "rejected-cleanup"
			f.backend.conflict.Store(mode == "rejected-cleanup")
			err := f.store.Update(t.Context(), func(tx *Tx) error { return tx.Put("one", 1) })
			var first *RequestError
			if !errors.As(err, &first) {
				t.Fatalf("missing recovery identity: %v", err)
			}
			before := f.backend.commits.Load()
			inserts := f.pending.inserts
			called := false
			err = f.store.Update(t.Context(), func(tx *Tx) error { called = true; return tx.Put("different", 2) })
			var second *RequestError
			if called || !errors.As(err, &second) || second.Key != first.Key || second.StoreID != first.StoreID ||
				f.backend.commits.Load() != before || f.pending.inserts != inserts {
				t.Fatalf("fresh callback/request after pending: called=%v err=%v", called, err)
			}
			f.reopen()
			err = f.store.Update(t.Context(), func(tx *Tx) error { called = true; return nil })
			if called || !errors.As(err, &second) || second.Key != first.Key {
				t.Fatalf("guard lost on reopen: %v", err)
			}
			if mode == "unknown-durable" || mode == "durable-cleanup" || mode == "unattempted" {
				f.backend.loseReply.Store(false)
				f.backend.failLookup.Store(false)
				f.pending.failCleanup = false
				must(t, f.store.Resume(t.Context(), first.Key))
				must(t, f.store.Update(t.Context(), func(tx *Tx) error { called = true; return tx.Put("two", 2) }))
				if !called {
					t.Fatal("explicit recovery did not unblock")
				}
			}
		})
	}
}

func TestStoreRejectsLateCallbackAndPreservesSuccessfulResult(t *testing.T) {
	f := newFixture(t)
	var late func(*client.Client) error
	f.store.options.Process = processFunc(func(_ context.Context, fn func(*client.Client) error) error {
		late = fn
		return context.DeadlineExceeded
	})
	called := false
	err := f.store.Update(t.Context(), func(*Tx) error { called = true; return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if err := late(nil); !errors.Is(err, ErrClosed) || called {
		t.Fatalf("late callback ran: %v", err)
	}
	f.store.options.Process = processFunc(func(ctx context.Context, fn func(*client.Client) error) error {
		if err := f.process.WithClient(ctx, fn); err != nil {
			return err
		}
		return errors.New("subprocess shutdown failed after durable acknowledgement")
	})
	must(t, f.store.Update(t.Context(), func(tx *Tx) error { return tx.Create("once", 1) }))
	if f.backend.commits.Load() != 1 || len(f.pending.rows) != 0 || readInt(t, f.store, "once") != 1 {
		t.Fatal("known durable result was not preserved")
	}
}

func TestGuardSkipsOtherScopes(t *testing.T) {
	f := newFixture(t)
	var record aterequest.Record
	must(t, f.store.View(t.Context(), func(tx *Tx) error {
		body, err := api.EncodeStateBatch(api.StateBatch{Operations: []api.StateMutation{{
			Key: []byte("x"), Kind: api.MutationPut, Value: api.Value{Kind: api.ValueBytes, Data: []byte("1")},
		}}})
		if err != nil {
			return err
		}
		record, err = aterequest.NewRecord(tx.view.Anchor.Fence.Store, api.CommitRequest{
			Space: "aaa-other", Principal: "gsea", RequestID: "unrelated", Method: api.MethodMutateState, Body: body,
		})
		return err
	}))
	must(t, f.pending.Insert(t.Context(), record))
	must(t, f.store.Update(t.Context(), func(tx *Tx) error { return tx.Put("ok", 1) }))
	if len(f.pending.rows) != 1 {
		t.Fatal("unrelated recovery payload changed")
	}
}

type receiptPending struct {
	aterequest.PendingStore
	record aterequest.Record
}

func (p receiptPending) List(_ context.Context, options aterequest.ListOptions) ([]aterequest.Record, error) {
	if options.After != nil {
		return nil, nil
	}
	return []aterequest.Record{p.record}, nil
}

func TestPendingGuardPreservesSavedRejectionClassification(t *testing.T) {
	record, err := aterequest.NewRecord("native", api.CommitRequest{
		Space: "authority", Principal: "gsea", RequestID: "rejected", Method: api.MethodMutateState,
	})
	must(t, err)
	record.Attempted = true
	result := api.CommitResult{Code: api.CodeConflict, Conflict: api.ConflictPrecondition, RequestID: record.Request.RequestID}
	record.Rejection = &result
	store := &Store{options: Options{Space: "authority", Principal: "gsea", Pending: receiptPending{record: record}}}
	err = store.reconcilePending(t.Context(), nil, record.StoreID)
	var request *RequestError
	if !errors.As(err, &request) || request.Key != record.Request.Key() || request.Result != result ||
		!errors.Is(err, ErrConflict) || !errors.Is(err, aterequest.ErrRejected) ||
		!errors.Is(err, aterequest.ErrRejectionCleanup) || errors.Is(err, aterequest.ErrUnknown) {
		t.Fatalf("saved rejection misclassified: %v", err)
	}
}

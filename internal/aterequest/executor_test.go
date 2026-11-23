package aterequest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/agent-temporal-engine/protocol"
)

type leaseProcess func(context.Context, func(*client.Client) error) error

func (p leaseProcess) WithClient(ctx context.Context, fn func(*client.Client) error) error {
	return p(ctx, fn)
}

func TestLeaseRejectsLateAndRepeatedCallbacks(t *testing.T) {
	var late func(*client.Client) error
	calls := 0
	process := leaseProcess(func(_ context.Context, fn func(*client.Client) error) error {
		late = fn
		return context.DeadlineExceeded
	})
	fn := func(context.Context, *client.Client) error { calls++; return nil }
	if err := WithClient(t.Context(), process, fn); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if err := late(nil); !errors.Is(err, ErrLeaseClosed) || calls != 0 {
		t.Fatalf("late work accepted: calls=%d err=%v", calls, err)
	}
	process = leaseProcess(func(_ context.Context, fn func(*client.Client) error) error {
		if err := fn(nil); err != nil {
			return err
		}
		if err := fn(nil); !errors.Is(err, ErrLeaseClosed) {
			t.Fatalf("repeated callback accepted: %v", err)
		}
		return errors.New("shutdown failed after completed work")
	})
	if err := WithClient(t.Context(), process, fn); err != nil || calls != 1 {
		t.Fatalf("completed outcome lost: calls=%d err=%v", calls, err)
	}
}

func TestLeaseCancelsAndJoinsInterruptedCallback(t *testing.T) {
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	process := leaseProcess(func(_ context.Context, fn func(*client.Client) error) error {
		go func() { _ = fn(nil) }()
		<-started
		return context.DeadlineExceeded
	})
	result := 0
	done := make(chan error, 1)
	go func() {
		done <- WithClient(t.Context(), process, func(ctx context.Context, _ *client.Client) error {
			close(started)
			<-ctx.Done()
			close(canceled)
			<-release
			result = 42
			return ctx.Err()
		})
	}()
	<-canceled
	select {
	case err := <-done:
		t.Fatalf("returned while callback owns result: %v", err)
	default:
	}
	close(release)
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, context.Canceled) || result != 42 {
		t.Fatalf("interrupted callback not joined: result=%d err=%v", result, err)
	}
}

func TestLeasePreservesReceiptsAfterProcessReturns(t *testing.T) {
	for _, outcome := range []string{"ack", "ack-cleanup", "rejection-cleanup"} {
		t.Run(outcome, func(t *testing.T) {
			record := requestFixture(t)
			receipt := requestAck(record)
			var callbackErr error
			switch outcome {
			case "ack-cleanup":
				callbackErr = &CleanupError{Key: record.Request.Key(), Result: receipt, Cause: errors.New("cleanup interrupted")}
			case "rejection-cleanup":
				receipt = api.CommitResult{Code: api.CodeRejected, RequestID: record.Request.RequestID}
				callbackErr = &RejectionCleanupError{Key: record.Request.Key(), Result: receipt, Cause: errors.New("cleanup interrupted")}
			}
			started := make(chan struct{})
			process := leaseProcess(func(_ context.Context, fn func(*client.Client) error) error {
				go func() { _ = fn(nil) }()
				<-started
				return context.DeadlineExceeded
			})
			var got api.CommitResult
			err := WithClient(t.Context(), process, func(ctx context.Context, _ *client.Client) error {
				close(started)
				// The process must return before the lease cancels this context.
				// This exercises joining without waiting under the lease mutex.
				<-ctx.Done()
				got = receipt
				return callbackErr
			})
			if got != receipt || errors.Is(err, ErrUnknown) {
				t.Fatalf("known receipt lost: result=%+v err=%v", got, err)
			}
			switch outcome {
			case "ack":
				if err != nil {
					t.Fatalf("durable acknowledgement became failure: %v", err)
				}
			case "ack-cleanup":
				var cleanup *CleanupError
				if !errors.As(err, &cleanup) || cleanup.Result != receipt || !errors.Is(err, ErrCleanup) {
					t.Fatalf("acknowledged cleanup evidence lost: %v", err)
				}
			case "rejection-cleanup":
				var cleanup *RejectionCleanupError
				if !errors.As(err, &cleanup) || cleanup.Result != receipt ||
					!errors.Is(err, ErrRejected) || !errors.Is(err, ErrRejectionCleanup) {
					t.Fatalf("rejection cleanup evidence lost: %v", err)
				}
			}
		})
	}
}

type listTestStore struct {
	PendingStore
	list func(ListOptions) ([]Record, error)
}

func (s listTestStore) List(_ context.Context, options ListOptions) ([]Record, error) {
	return s.list(options)
}

func TestPendingInventoryRequiresCompleteBoundedOrderedPage(t *testing.T) {
	if MaxListRecords != 128 || MaxListBytes != 32<<20 {
		t.Fatal("pending admission bounds changed")
	}
	record := requestFixture(t)
	for _, mode := range []string{"complete", "short-byte-page", "duplicate", "count", "bytes"} {
		t.Run(mode, func(t *testing.T) {
			store := listTestStore{list: func(options ListOptions) ([]Record, error) {
				if err := options.Validate(); err != nil {
					t.Fatal(err)
				}
				if options.After != nil {
					if *options.After != record.Request.Key() || options.Limit != 1 {
						t.Fatal("incorrect exhaustion probe")
					}
					if mode == "short-byte-page" {
						return []Record{record}, nil
					}
					return nil, nil
				}
				switch mode {
				case "duplicate":
					return []Record{record, record}, nil
				case "count":
					return make([]Record, MaxListRecords+1), nil
				case "bytes":
					var records []Record
					for _, id := range []model.RequestID{"a", "b", "c", "d", "e", "f", "g"} {
						r := record.Request
						r.RequestID, r.Body = id, make([]byte, 4<<20)
						value, err := NewRecord(record.StoreID, r)
						if err != nil {
							t.Fatal(err)
						}
						records = append(records, value)
					}
					return records, nil
				default:
					return []Record{record}, nil
				}
			}}
			records, err := ListPending(t.Context(), store)
			if mode == "complete" {
				if err != nil || len(records) != 1 {
					t.Fatal(err)
				}
			} else if records != nil || err == nil {
				t.Fatalf("incomplete or invalid inventory admitted: %v", err)
			}
		})
	}
}

type (
	requestTestStore struct {
		PendingStore
		record    Record
		found     bool
		deleted   bool
		rejectErr error
		transform func(Record) Record
	}

	requestTestClient struct {
		commit func(context.Context, api.CommitRequest) (api.CommitResult, error)
		lookup func(context.Context, api.LookupRequestRequest) (api.LookupRequestResult, error)
		calls  int
	}
)

func (store *requestTestStore) Insert(_ context.Context, record Record) error {
	if store.record.Rejection != nil {
		return ErrRejected
	}
	if store.found {
		if !Equal(store.record, record) {
			return ErrConflict
		}
		return nil
	}
	store.record, store.found = record, true
	return nil
}

func (store *requestTestStore) BeginAttempt(_ context.Context, record Record) (bool, error) {
	if !store.found || !Equal(record, store.record) {
		return false, ErrConflict
	}
	if store.record.Attempted {
		return false, nil
	}
	store.record.Attempted = true
	return true, nil
}

func (store *requestTestStore) DeleteRejected(_ context.Context, record Record, result api.CommitResult) error {
	if err := VerifyRejection(record, result); err != nil {
		return err
	}
	if store.rejectErr != nil {
		return store.rejectErr
	}
	store.record.Rejection = &result
	store.deleted, store.found = true, false
	return nil
}

func (store *requestTestStore) Get(context.Context, api.RequestKey) (Record, bool, error) {
	record := store.record
	if store.transform != nil {
		record = store.transform(record)
	}
	return record, store.found, nil
}

func (store *requestTestStore) DeleteAcknowledged(_ context.Context, record Record, result api.CommitResult) error {
	if err := VerifyAcknowledgement(record, result); err != nil {
		return err
	}
	store.deleted, store.found = true, false
	return nil
}

func (client *requestTestClient) Commit(ctx context.Context, request api.CommitRequest) (api.CommitResult, error) {
	client.calls++
	return client.commit(ctx, request)
}

func (client *requestTestClient) LookupRequest(ctx context.Context, request api.LookupRequestRequest) (api.LookupRequestResult, error) {
	client.calls++
	return client.lookup(ctx, request)
}

func requestFixture(t *testing.T) Record {
	t.Helper()
	record, err := NewRecord("store", api.CommitRequest{
		Space: "space", Principal: "principal", RequestID: "stable-request",
		Method: "native/v1", Body: []byte{0, 0xff, 0x80, '\n'},
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func requestAck(record Record) api.CommitResult {
	return api.CommitResult{
		Code: api.CodeDurableAck, RequestID: record.Request.RequestID, RequestDigest: record.Digest,
		Fence: model.CommitFence{Store: record.StoreID, LSN: 1, CommitDigest: model.CommitDigest(strings.Repeat("a", 64))},
	}
}

func absentLookup(context.Context, api.LookupRequestRequest) (api.LookupRequestResult, error) {
	return api.LookupRequestResult{Code: api.CodeNotFoundAtFence, ObservedFence: model.CommitFence{
		Store: "store", CommitDigest: model.CommitDigest(strings.Repeat("0", 64)),
	}}, nil
}

func TestExecutorValidatesSavedIdentityBeforeNetwork(t *testing.T) {
	for name, transform := range map[string]func(Record) Record{
		"body": func(r Record) Record { r.Request.Body = []byte("corrupt"); return r },
		"key": func(r Record) Record {
			r.Request.RequestID = "another-request"
			r.Digest, _ = api.CommandDigest(r.Request)
			return r
		},
		"store": func(r Record) Record { r.StoreID = "another-store"; return r },
		"valid-but-different-envelope": func(r Record) Record {
			r.Request.Method = "changed/v1"
			r.Digest, _ = api.CommandDigest(r.Request)
			return r
		},
	} {
		t.Run(name, func(t *testing.T) {
			record := requestFixture(t)
			store := &requestTestStore{transform: transform}
			client := &requestTestClient{}
			executor, err := NewExecutor(client, store, record.StoreID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := executor.Execute(t.Context(), record.Request); err == nil || client.calls != 0 || store.deleted {
				t.Fatalf("unverified request reached network: calls=%d deleted=%v err=%v", client.calls, store.deleted, err)
			}
		})
	}
}

func TestReconcileAcknowledgedNeverSubmits(t *testing.T) {
	for _, attempted := range []bool{false, true} {
		record := requestFixture(t)
		record.Attempted = attempted
		store := &requestTestStore{record: record, found: true}
		client := &requestTestClient{lookup: absentLookup, commit: func(context.Context, api.CommitRequest) (api.CommitResult, error) {
			t.Fatal("ACK-only reconciliation submitted a command")
			return api.CommitResult{}, nil
		}}
		executor, err := NewExecutor(client, store, record.StoreID)
		if err != nil {
			t.Fatal(err)
		}
		result, err := executor.ReconcileAcknowledged(t.Context(), record.Request.Key())
		if err != nil || result.Code != api.CodeNotFoundAtFence || !store.found || store.record.Attempted != attempted {
			t.Fatalf("absence changed pending: %+v %v", result, err)
		}
		client.lookup = func(context.Context, api.LookupRequestRequest) (api.LookupRequestResult, error) {
			ack := requestAck(record)
			return api.LookupRequestResult{Code: api.CodeDurableAck, Result: ack, ObservedFence: ack.Fence}, nil
		}
		if _, err := executor.ReconcileAcknowledged(t.Context(), record.Request.Key()); err != nil || store.found {
			t.Fatalf("verified ACK not cleaned: %v", err)
		}
	}
}

func TestReconcileAcknowledgedFinishesSavedRejectionLocally(t *testing.T) {
	record := requestFixture(t)
	record.Attempted = true
	rejection := api.CommitResult{Code: api.CodeRejected, RequestID: record.Request.RequestID}
	record.Rejection = &rejection
	failure := errors.New("injected rejection cleanup failure")
	store := &requestTestStore{record: record, found: true, rejectErr: failure}
	// Any network access is a bug: the persisted terminal rejection already
	// proves the outcome, even if the authority is currently unavailable.
	client := &requestTestClient{
		lookup: func(context.Context, api.LookupRequestRequest) (api.LookupRequestResult, error) {
			t.Fatal("saved rejection required a network lookup")
			return api.LookupRequestResult{}, nil
		},
		commit: func(context.Context, api.CommitRequest) (api.CommitResult, error) {
			t.Fatal("saved rejection was resubmitted")
			return api.CommitResult{}, nil
		},
	}
	executor, err := NewExecutor(client, store, record.StoreID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.ReconcileAcknowledged(t.Context(), record.Request.Key())
	if result != rejection || !errors.Is(err, ErrRejectionCleanup) || !errors.Is(err, failure) ||
		!store.found || store.deleted || client.calls != 0 {
		t.Fatalf("cleanup failure lost original rejection: result=%+v err=%v", result, err)
	}
	store.rejectErr = nil
	result, err = executor.ReconcileAcknowledged(t.Context(), record.Request.Key())
	if result != rejection || !errors.Is(err, ErrRejected) || errors.Is(err, ErrRejectionCleanup) ||
		store.found || !store.deleted || client.calls != 0 {
		t.Fatalf("local cleanup did not finish: result=%+v err=%v", result, err)
	}
	if err := store.Insert(t.Context(), record); !errors.Is(err, ErrRejected) {
		t.Fatalf("cleaned rejection became replayable: %v", err)
	}
}

func TestExecutorRejectsUnverifiedAcknowledgements(t *testing.T) {
	for name, alter := range map[string]func(*api.CommitResult){
		"digest":   func(r *api.CommitResult) { r.RequestDigest = model.ObjectID(strings.Repeat("b", 64)) },
		"request":  func(r *api.CommitResult) { r.RequestID = "wrong" },
		"store":    func(r *api.CommitResult) { r.Fence.Store = "wrong" },
		"fence":    func(r *api.CommitResult) { r.Fence.CommitDigest = "bad" },
		"zero-lsn": func(r *api.CommitResult) { r.Fence.LSN = 0 },
		"volatile": func(r *api.CommitResult) { r.Code = api.CodeVolatileAck },
	} {
		for _, viaLookup := range []bool{false, true} {
			suffix := "/commit"
			if viaLookup {
				suffix = "/lookup"
			}
			t.Run(name+suffix, func(t *testing.T) {
				record := requestFixture(t)
				bad := requestAck(record)
				alter(&bad)
				store := &requestTestStore{}
				client := &requestTestClient{
					lookup: absentLookup,
					commit: func(context.Context, api.CommitRequest) (api.CommitResult, error) { return bad, nil },
				}
				if viaLookup {
					client.lookup = func(context.Context, api.LookupRequestRequest) (api.LookupRequestResult, error) {
						return api.LookupRequestResult{Code: api.CodeDurableAck, Result: bad, ObservedFence: requestAck(record).Fence}, nil
					}
				}
				executor, _ := NewExecutor(client, store, record.StoreID)
				result, err := executor.Execute(t.Context(), record.Request)
				if !errors.Is(err, ErrUnknown) || result.Code == api.CodeDurableAck || store.deleted || !store.found {
					t.Fatalf("bad ack accepted: result=%+v err=%v deleted=%v", result, err, store.deleted)
				}
			})
		}
	}
}

func TestExecuteFinishesInterruptedRejectionWithoutNetwork(t *testing.T) {
	record := requestFixture(t)
	record.Attempted = true
	rejection := api.CommitResult{Code: api.CodeRejected, RequestID: record.Request.RequestID}
	record.Rejection = &rejection
	store := &requestTestStore{record: record, found: true}
	network := &requestTestClient{}
	executor, err := NewExecutor(network, store, record.StoreID)
	if err != nil {
		t.Fatal(err)
	}
	changed := record.Request
	changed.Body = []byte("changed")
	if _, err := executor.Execute(t.Context(), changed); !errors.Is(err, ErrConflict) || !store.found {
		t.Fatalf("changed request used saved rejection: %v", err)
	}
	result, err := executor.Execute(t.Context(), record.Request)
	if result != rejection || !errors.Is(err, ErrRejected) || errors.Is(err, ErrRejectionCleanup) ||
		store.found || !store.deleted || network.calls != 0 {
		t.Fatalf("saved rejection not cleaned locally: result=%+v err=%v", result, err)
	}
	if _, err := executor.Execute(t.Context(), record.Request); !errors.Is(err, ErrRejected) || network.calls != 0 {
		t.Fatalf("terminal identity reused: %v", err)
	}
}

func TestExecutorUncertainOutcomesRemainPending(t *testing.T) {
	for _, code := range []api.ResultCode{api.CodeCommitUnknown, api.CodeWaitInterrupted, api.CodeNotSubmitted, api.CodeInFlight, api.CodeIndeterminate, api.CodeRejected} {
		t.Run(string(code), func(t *testing.T) {
			record := requestFixture(t)
			store := &requestTestStore{}
			client := &requestTestClient{
				lookup: absentLookup,
				commit: func(context.Context, api.CommitRequest) (api.CommitResult, error) {
					return api.CommitResult{Code: code, RequestID: record.Request.RequestID}, context.Canceled
				},
			}
			executor, _ := NewExecutor(client, store, record.StoreID)
			result, err := executor.Execute(t.Context(), record.Request)
			if !errors.Is(err, ErrUnknown) || !errors.Is(err, context.Canceled) || errors.Is(err, ErrRejected) ||
				result.Code != api.CodeCommitUnknown || !store.found || store.deleted {
				t.Fatalf("uncertainty lost: result=%+v err=%v", result, err)
			}
		})
	}
}

func TestExecutorLookupMustProveAbsenceAtBoundStore(t *testing.T) {
	for _, code := range []api.ResultCode{api.CodeInFlight, api.CodeIndeterminate, api.CodeVolatileNotFound, api.CodeNotFoundAtFence} {
		t.Run(string(code), func(t *testing.T) {
			record := requestFixture(t)
			store := &requestTestStore{}
			client := &requestTestClient{
				lookup: func(context.Context, api.LookupRequestRequest) (api.LookupRequestResult, error) {
					out, _ := absentLookup(context.Background(), api.LookupRequestRequest{})
					out.Code = code
					out.ObservedFence.Store = "wrong"
					return out, nil
				},
			}
			executor, _ := NewExecutor(client, store, record.StoreID)
			_, err := executor.Execute(t.Context(), record.Request)
			if !errors.Is(err, ErrUnknown) || client.calls != 1 || store.deleted {
				t.Fatalf("lookup allowed unsafe retry: calls=%d err=%v", client.calls, err)
			}
		})
	}
}

func TestExecutorCanceledContextDoesNotBecomeRejection(t *testing.T) {
	record := requestFixture(t)
	store := &requestTestStore{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := &requestTestClient{
		lookup: absentLookup,
		commit: func(context.Context, api.CommitRequest) (api.CommitResult, error) {
			cancel()
			return api.CommitResult{Code: api.CodeRejected, RequestID: record.Request.RequestID}, nil
		},
	}
	executor, _ := NewExecutor(client, store, record.StoreID)
	_, err := executor.Execute(ctx, record.Request)
	if !errors.Is(err, ErrUnknown) || !errors.Is(err, context.Canceled) || errors.Is(err, ErrRejected) || store.deleted {
		t.Fatalf("canceled context classified as rejection: %v", err)
	}
}

func TestExecutorPartialErrorResponseIsUnknown(t *testing.T) {
	record := requestFixture(t)
	store := &requestTestStore{}
	cause := errors.New("response decode failed after reading result code")
	client := &requestTestClient{
		lookup: absentLookup,
		commit: func(context.Context, api.CommitRequest) (api.CommitResult, error) {
			return api.CommitResult{Code: api.CodeRejected, RequestID: record.Request.RequestID}, cause
		},
	}
	executor, _ := NewExecutor(client, store, record.StoreID)
	_, err := executor.Execute(t.Context(), record.Request)
	if !errors.Is(err, ErrUnknown) || !errors.Is(err, cause) || errors.Is(err, ErrRejected) || store.deleted {
		t.Fatalf("partial error response classified as rejection: %v", err)
	}
}

func TestExecutorReconcilesLostAckImmediately(t *testing.T) {
	record := requestFixture(t)
	store := &requestTestStore{}
	committed := false
	client := &requestTestClient{
		lookup: func(ctx context.Context, key api.LookupRequestRequest) (api.LookupRequestResult, error) {
			if !committed {
				return absentLookup(ctx, key)
			}
			ack := requestAck(record)
			return api.LookupRequestResult{Code: api.CodeDurableAck, Result: ack, ObservedFence: ack.Fence}, nil
		},
		commit: func(context.Context, api.CommitRequest) (api.CommitResult, error) {
			committed = true
			return api.CommitResult{Code: api.CodeCommitUnknown}, context.DeadlineExceeded
		},
	}
	executor, _ := NewExecutor(client, store, record.StoreID)
	result, err := executor.Execute(t.Context(), record.Request)
	if err != nil || result != requestAck(record) || !store.deleted || client.calls != 3 {
		t.Fatalf("lost ack not reconciled: result=%+v err=%v calls=%d", result, err, client.calls)
	}
}

func TestExecutorExpiredSubmissionResumesOnlyOriginalReceipt(t *testing.T) {
	for _, acknowledged := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "acknowledged"}[acknowledged], func(t *testing.T) {
			record := requestFixture(t)
			store := &requestTestStore{}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
			defer cancel()
			submissions := 0
			lookups := 0
			native := &requestTestClient{
				lookup: func(ctx context.Context, request api.LookupRequestRequest) (api.LookupRequestResult, error) {
					lookups++
					if ctx.Err() != nil {
						t.Fatal("lookup reused expired submission context")
					}
					if submissions > 0 && acknowledged {
						ack := requestAck(record)
						return api.LookupRequestResult{Code: api.CodeDurableAck, Result: ack, ObservedFence: ack.Fence}, nil
					}
					return absentLookup(ctx, request)
				},
				commit: func(ctx context.Context, request api.CommitRequest) (api.CommitResult, error) {
					submissions++
					if request.Key() != record.Request.Key() || !bytes.Equal(request.Body, record.Request.Body) {
						t.Fatal("original command changed")
					}
					<-ctx.Done()
					return api.CommitResult{}, ctx.Err()
				},
			}
			executor, _ := NewExecutor(native, store, record.StoreID)
			_, err := executor.Execute(ctx, record.Request)
			if !errors.Is(err, ErrUnknown) || !errors.Is(err, context.DeadlineExceeded) || lookups != 1 {
				t.Fatalf("expired submission: err=%v lookups=%d", err, lookups)
			}
			result, err := executor.Resume(t.Context(), record.Request.Key())
			if acknowledged {
				if err != nil || result != requestAck(record) || !store.deleted {
					t.Fatalf("fresh receipt recovery failed: %v %+v", err, result)
				}
			} else if !errors.Is(err, ErrUnknown) || store.deleted {
				t.Fatalf("absence incorrectly resolved: %v", err)
			}
			if submissions != 1 || lookups != 2 {
				t.Fatalf("original command resubmitted: submissions=%d lookups=%d", submissions, lookups)
			}
		})
	}
}

func TestExecutorExplicitRejectionDeletesOriginal(t *testing.T) {
	record := requestFixture(t)
	store := &requestTestStore{}
	client := &requestTestClient{
		lookup: absentLookup,
		commit: func(context.Context, api.CommitRequest) (api.CommitResult, error) {
			return api.CommitResult{Code: api.CodeRejected, RequestID: record.Request.RequestID}, nil
		},
	}
	executor, _ := NewExecutor(client, store, record.StoreID)
	_, err := executor.Execute(t.Context(), record.Request)
	if !errors.Is(err, ErrRejected) || errors.Is(err, ErrUnknown) || store.found || !store.deleted {
		t.Fatalf("rejection classification: %v", err)
	}
	calls := client.calls
	if _, err := executor.Resume(t.Context(), record.Request.Key()); !errors.Is(err, ErrNotFound) || client.calls != calls {
		t.Fatalf("rejected request replayed: %v", err)
	}
}

func TestExecutorMalformedRejectionsRetainPayload(t *testing.T) {
	record := requestFixture(t)
	for name, alter := range map[string]func(*api.CommitResult){
		"wrong-id":          func(r *api.CommitResult) { r.RequestID = "other" },
		"unknown-conflict":  func(r *api.CommitResult) { r.Code, r.Conflict = api.CodeConflict, "other" },
		"missing-conflict":  func(r *api.CommitResult) { r.Code = api.CodeConflict },
		"rejected-conflict": func(r *api.CommitResult) { r.Conflict = api.ConflictPrecondition },
		"matching-digest":   func(r *api.CommitResult) { r.RequestDigest = record.Digest },
		"wrong-digest":      func(r *api.CommitResult) { r.RequestDigest = "wrong" },
		"publication":       func(r *api.CommitResult) { r.Fence = requestAck(record).Fence },
		"transaction":       func(r *api.CommitResult) { r.TxID = "transaction" },
		"root":              func(r *api.CommitResult) { r.RootSet = requestAck(record).RequestDigest },
	} {
		t.Run(name, func(t *testing.T) {
			result := api.CommitResult{Code: api.CodeRejected, RequestID: record.Request.RequestID}
			alter(&result)
			store := &requestTestStore{}
			client := &requestTestClient{lookup: absentLookup,
				commit: func(context.Context, api.CommitRequest) (api.CommitResult, error) { return result, nil }}
			executor, _ := NewExecutor(client, store, record.StoreID)
			out, err := executor.Execute(t.Context(), record.Request)
			if !errors.Is(err, ErrUnknown) || errors.Is(err, ErrRejected) || out.Code != api.CodeCommitUnknown || store.deleted || !store.found {
				t.Fatalf("malformed rejection removed bytes: result=%+v err=%v", out, err)
			}
			calls := client.calls
			if _, err := executor.Resume(t.Context(), record.Request.Key()); !errors.Is(err, ErrUnknown) || client.calls != calls+1 {
				t.Fatalf("ambiguous attempt resubmitted: %v calls=%d", err, client.calls)
			}
		})
	}
}

func TestExecutorNativeSemanticRejectionAndUnknownErrors(t *testing.T) {
	for _, class := range []api.ErrorClass{api.ErrorInvalidRequest, api.ErrorUnsupported, api.ErrorUnauthorized, api.ErrorConflict,
		api.ErrorCanceled, api.ErrorDeadline, api.ErrorCommitUnknown, api.ErrorInternal, api.ErrorStoreFenced} {
		t.Run(string(class), func(t *testing.T) {
			record := requestFixture(t)
			store := &requestTestStore{}
			client := &requestTestClient{lookup: absentLookup,
				commit: func(context.Context, api.CommitRequest) (api.CommitResult, error) {
					return api.CommitResult{Code: api.CodeRejected, RequestID: record.Request.RequestID}, &protocol.Error{Code: class}
				}}
			executor, _ := NewExecutor(client, store, record.StoreID)
			_, err := executor.Execute(t.Context(), record.Request)
			conclusive := class == api.ErrorInvalidRequest || class == api.ErrorUnsupported || class == api.ErrorUnauthorized || class == api.ErrorConflict
			if store.deleted != conclusive || errors.Is(err, ErrRejected) != conclusive || errors.Is(err, ErrUnknown) == conclusive {
				t.Fatalf("incorrect semantic error handling: deleted=%v err=%v", store.deleted, err)
			}
		})
	}
}

func TestRecordStrictCodecAndImmutableBytes(t *testing.T) {
	original := requestFixture(t)
	request := original.Request
	request.ExpectedFence = &model.CommitFence{Store: "store", CommitDigest: model.CommitDigest(strings.Repeat("0", 64))}
	record, err := NewRecord("store", request)
	if err != nil {
		t.Fatal(err)
	}
	request.Body[0] = 99
	request.ExpectedFence.LSN = 99
	if record.Request.Body[0] != 0 || record.Request.ExpectedFence.LSN != 0 {
		t.Fatal("record aliases caller-owned memory")
	}
	raw, err := EncodeRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeRecord(raw)
	if err != nil || !Equal(decoded, record) || !bytes.Equal(decoded.Request.Body, []byte{0, 0xff, 0x80, '\n'}) {
		t.Fatalf("opaque bytes did not round trip: %v", err)
	}
	for name, invalid := range map[string][]byte{
		"duplicate": append([]byte(`{"version":1,`), raw[1:]...),
		"unknown":   append([]byte(`{"unexpected":1,`), raw[1:]...),
		"trailing":  append(bytes.Clone(raw), []byte(`{}`)...),
		"digest":    bytes.Replace(raw, []byte(record.Digest), []byte(strings.Repeat("c", 64)), 1),
		"version":   bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":2`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeRecord(invalid); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("invalid internal record accepted: %v", err)
			}
		})
	}
}

func TestRecordCanonicalDigestAndLifecycleSeparation(t *testing.T) {
	record := requestFixture(t)
	canonical, err := api.CanonicalCommand(record.Request)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(canonical)
	if string(record.Digest) != hex.EncodeToString(digest[:]) {
		t.Fatal("record digest differs from the native canonical bytes")
	}
	raw, err := EncodeRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	saved := record
	saved.Attempted = true
	result := api.CommitResult{Code: api.CodeRejected, RequestID: record.Request.RequestID}
	saved.Rejection = &result
	savedRaw, err := EncodeRecord(saved)
	if err != nil || !Equal(record, saved) || !bytes.Equal(raw, savedRaw) {
		t.Fatalf("lifecycle metadata changed immutable identity: %v", err)
	}
	if err := VerifyAcknowledgement(saved, requestAck(record)); err == nil {
		t.Fatal("saved rejection accepted a contradictory ACK")
	}
	changed := result
	changed.Code, changed.Conflict = api.CodeConflict, api.ConflictPrecondition
	if err := VerifyRejection(saved, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("saved rejection accepted a different terminal result: %v", err)
	}
	saved.Attempted = false
	if err := saved.Validate(); err == nil {
		t.Fatal("rejection without durable attempt accepted")
	}
	changedRecord := record
	changedRecord.Request.Body = []byte("changed with the original digest")
	if Equal(record, changedRecord) || changedRecord.Validate() == nil {
		t.Fatal("changed bytes concealed by the original digest")
	}
	for name, malformed := range map[string][]byte{
		"nested-duplicate": bytes.Replace(raw, []byte(`"Method":`), []byte(`"Method":"ignored","Method":`), 1),
		"nested-unknown":   bytes.Replace(raw, []byte(`"request":{`), []byte(`"request":{"extra":true,`), 1),
		"case-alias":       bytes.Replace(raw, []byte(`"RequestID":`), []byte(`"requestid":`), 1),
		"null-version":     bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":null`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeRecord(malformed); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("ambiguous record accepted: %v", err)
			}
		})
	}
}

func TestRecordNativeSizeAndIdentityBounds(t *testing.T) {
	request := requestFixture(t).Request
	request.Body = bytes.Repeat([]byte{0xff}, api.MaxCommandBody)
	record, err := NewRecord("store", request)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := EncodeRecord(record)
	if err != nil || len(raw) > MaxRecordBytes || len(raw) > MaxListBytes {
		t.Fatalf("maximum native command cannot fit in recovery: size=%d err=%v", len(raw), err)
	}
	decoded, err := DecodeRecord(raw)
	if err != nil || !Equal(record, decoded) {
		t.Fatalf("maximum native command did not round trip: %v", err)
	}
	request.Body = append(request.Body, 0)
	if _, err := NewRecord("store", request); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversize command accepted: %v", err)
	}
	request = requestFixture(t).Request
	for _, store := range []model.StoreID{"", model.StoreID(strings.Repeat("s", api.MaxIdentityBytes+1)), "\xff"} {
		if _, err := NewRecord(store, request); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid store identity accepted: %v", err)
		}
	}
	request.ExpectedFence = &model.CommitFence{Store: "other", CommitDigest: model.CommitDigest(strings.Repeat("0", 64))}
	if _, err := NewRecord("store", request); !errors.Is(err, ErrInvalid) {
		t.Fatalf("foreign expected fence accepted: %v", err)
	}
}

func TestTerminalCodecRejectsAmbiguousOutcomes(t *testing.T) {
	record := requestFixture(t)
	for _, outcome := range []string{"rejected", "precondition", "idempotency", "retired"} {
		t.Run(outcome, func(t *testing.T) {
			terminal := Rejection{StoreID: record.StoreID, Key: record.Request.Key(), Digest: record.Digest}
			terminal.Result = api.CommitResult{Code: api.CodeRejected, RequestID: record.Request.RequestID}
			switch outcome {
			case "precondition":
				terminal.Result.Code, terminal.Result.Conflict = api.CodeConflict, api.ConflictPrecondition
			case "idempotency":
				terminal.Result.Code, terminal.Result.Conflict = api.CodeConflict, api.ConflictIdempotency
			case "retired":
				terminal.Retired, terminal.Result = true, api.CommitResult{}
			}
			raw, err := EncodeRejection(terminal)
			if err != nil {
				t.Fatal(err)
			}
			got, err := DecodeRejection(raw)
			if err != nil || got != terminal || bytes.Contains(raw, []byte(`"Body"`)) ||
				bytes.Contains(raw, []byte(`"request"`)) {
				t.Fatalf("invalid body-free terminal round trip: %+v %v", got, err)
			}
			for name, malformed := range map[string][]byte{
				"duplicate": append([]byte(`{"version":1,`), raw[1:]...),
				"unknown":   append([]byte(`{"body":"unexpected",`), raw[1:]...),
				"version":   bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":2`), 1),
				"trailing":  append(bytes.Clone(raw), []byte(`{}`)...),
				"digest":    bytes.Replace(raw, []byte(terminal.Digest), []byte(strings.ToUpper(string(terminal.Digest))), 1),
				"key":       bytes.Replace(raw, []byte(`"Space":"space"`), []byte(`"Space":""`), 1),
			} {
				t.Run(name, func(t *testing.T) {
					if _, err := DecodeRejection(malformed); !errors.Is(err, ErrCorrupt) {
						t.Fatalf("invalid terminal marker accepted: %v", err)
					}
				})
			}
			terminal.Retired = !terminal.Retired
			if _, err := EncodeRejection(terminal); err == nil {
				t.Fatal("retirement/rejection confusion accepted")
			}
		})
	}
	terminal := Rejection{StoreID: record.StoreID, Key: record.Request.Key(), Digest: record.Digest}
	for _, result := range []api.CommitResult{
		requestAck(record),
		{Code: api.CodeCommitUnknown, RequestID: record.Request.RequestID},
		{Code: api.CodeRejected, RequestID: "wrong"},
		{Code: api.CodeRejected, RequestID: record.Request.RequestID, ResultDigest: record.Digest},
		{Code: api.CodeRejected, RequestID: record.Request.RequestID, StateRoot: record.Digest},
	} {
		terminal.Result = result
		if _, err := EncodeRejection(terminal); err == nil {
			t.Fatalf("invalid terminal response accepted: %+v err=%v", result, err)
		}
	}
}

func TestListOptionsRequireBothBoundsAndValidCursor(t *testing.T) {
	for _, options := range []ListOptions{
		{}, {Limit: -1, MaxBytes: 1}, {Limit: MaxListRecords + 1, MaxBytes: 1},
		{Limit: 1}, {Limit: 1, MaxBytes: -1}, {Limit: 1, MaxBytes: MaxListBytes + 1},
	} {
		if err := options.Validate(); !errors.Is(err, ErrLimit) {
			t.Fatalf("unbounded list accepted: %+v %v", options, err)
		}
	}
	key := requestFixture(t).Request.Key()
	options := ListOptions{After: &key, Limit: MaxListRecords, MaxBytes: MaxListBytes}
	if err := options.Validate(); err != nil {
		t.Fatal(err)
	}
	key.RequestID = ""
	if err := options.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid cursor accepted: %v", err)
	}
}

func TestExecutorAttemptedAbsenceRemainsUnknown(t *testing.T) {
	record := requestFixture(t)
	record.Attempted = true
	store := &requestTestStore{record: record, found: true}
	client := &requestTestClient{
		lookup: absentLookup,
		commit: func(context.Context, api.CommitRequest) (api.CommitResult, error) {
			t.Fatal("attempted request was resubmitted")
			return api.CommitResult{}, nil
		},
	}
	executor, err := NewExecutor(client, store, record.StoreID)
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range []func() (api.CommitResult, error){
		func() (api.CommitResult, error) { return executor.Resume(t.Context(), record.Request.Key()) },
		func() (api.CommitResult, error) { return executor.Execute(t.Context(), record.Request) },
	} {
		result, err := run()
		if !errors.Is(err, ErrUnknown) || errors.Is(err, ErrRejected) || result.Code != api.CodeCommitUnknown ||
			!store.found || store.deleted || !store.record.Attempted {
			t.Fatalf("attempted absence resolved or removed: %+v %v", result, err)
		}
	}
	if client.calls != 2 {
		t.Fatalf("expected only two lookups, got %d calls", client.calls)
	}
}

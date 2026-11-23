// Payload integration tests use the native stdio fixture from native_test.go.
package atestore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/vkviyu/agent-temporal-engine/agentprofile"
	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/memory"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

func largePending(size int) session.PendingEvent {
	event := pendingEvent("large-call")
	event.Payload = json.RawMessage(" \n{\"number\":1.00, \"text\":\"<" + strings.Repeat("x", size) + ">\"}\t\n")
	return event
}

func TestNativeLargeEnvelopeRoundTripAndProvenance(t *testing.T) {
	f := newNativeFixture(t)
	ctx := t.Context()
	input := []session.PendingEvent{pendingEvent("small"), largePending(1300 << 10), pendingEvent("small")}
	original, err := f.store.Append(ctx, "large", 0, input...)
	if err != nil {
		nativeFailure(t, err)
	}
	if len(f.pending.rows) != 0 {
		t.Fatal("successful append retained local payloads")
	}
	f.reopen(t, newPending())
	loaded, err := f.store.Load(ctx, "large", 0, 10)
	if err != nil || !reflect.DeepEqual(original, loaded) {
		t.Fatalf("large envelope round trip: %v", err)
	}
	if !bytes.Equal(input[1].Payload, loaded[1].Payload) {
		t.Fatal("original raw payload changed")
	}
	matched, err := f.store.LoadByCorrelation(ctx, "large", "large-call")
	if err != nil || !reflect.DeepEqual(matched, original[1:2]) {
		t.Fatalf("large correlation: %v", err)
	}
	records, err := f.store.HistoryThrough(ctx, "large", 0, 3, 10)
	if err != nil || len(records) != 3 {
		t.Fatalf("large frozen history: %v", err)
	}
	sources, err := f.store.EvidenceSources(ctx, records[1])
	if err != nil {
		nativeFailure(t, err)
	}
	if err := f.process.WithClient(ctx, func(c *client.Client) error {
		page, err := c.ListSessionEvents(ctx, api.ListSessionEventsRequest{Space: f.store.space, Principal: "gsea", SessionID: "large", AfterSequence: 1, ThroughSequence: 2, Limit: 1})
		if err != nil {
			return err
		}
		item := page.Items[0]
		if !item.Event.Event.PayloadRef.Present() || len(item.Event.Event.Payload) != 0 ||
			len(item.Event.Event.PayloadRef.Chunks) < 2 || sources[0].SourceID != item.Payload {
			return errors.New("source does not identify the existing native manifest event")
		}
		var raw bytes.Buffer
		if err := c.ReadSessionEventPayload(ctx, item, &raw); err != nil {
			return err
		}
		if !bytes.Equal(raw.Bytes(), records[1].Payload) {
			return errors.New("history differs from native payload bytes")
		}
		candidate := memory.Memory{Space: f.store.space, Kind: memory.KindFact, Claim: []byte("large native evidence"), Sources: sources}
		ack, err := c.ApplyMemory(ctx, api.ApplyMemoryRequest{Key: model.RequestKey{Space: f.store.space, Principal: "gsea", RequestID: "large-evidence"},
			Command: memory.Command{PutCandidate: &candidate}})
		if err != nil {
			return err
		}
		if ack.Code != api.CodeDurableAck {
			return errors.New("native provenance rejected")
		}
		return nil
	}); err != nil {
		nativeFailure(t, err)
	}
}

func TestNativeLargeAppendFailureRecovery(t *testing.T) {
	for _, mode := range []string{"upload-before", "upload-after", "before", "after"} {
		t.Run(mode, func(t *testing.T) {
			f := newNativeFixture(t)
			ctx := t.Context()
			input := []session.PendingEvent{largePending(400 << 10), pendingEvent("tail")}
			t.Setenv(dropEnvironment, mode)
			if _, err := f.store.Append(ctx, "recover-large", session.AnySequence, input...); !errors.Is(err, aterequest.ErrUnknown) {
				nativeFailure(t, err)
			}
			t.Setenv(dropEnvironment, "")
			pending, err := f.pending.List(ctx, aterequest.ListOptions{Limit: 128, MaxBytes: aterequest.MaxListBytes})
			if err != nil {
				t.Fatal(err)
			}
			wantPending := 1
			if strings.HasPrefix(mode, "upload-") {
				wantPending = 2
			}
			if len(pending) != wantPending {
				t.Fatalf("pending=%d want %d", len(pending), wantPending)
			}
			var plan appendPlan
			for _, record := range pending {
				r := record.Request
				if r.Method == api.MethodMutateState {
					plan.Upload = &r
				} else {
					plan.Request = r
				}
			}
			var original []session.Event
			if err := f.process.WithClient(ctx, func(c *client.Client) error {
				var err error
				original, err = f.store.recoverEvents(ctx, c, plan)
				return err
			}); err != nil {
				nativeFailure(t, err)
			}
			sequence, err := f.store.CurrentSequence(ctx, "recover-large")
			wantSequence := uint64(0)
			if mode == "after" {
				wantSequence = 2
			}
			if err != nil || sequence != wantSequence {
				t.Fatalf("partial append: %d %v", sequence, err)
			}
			reopenedPending := newPending()
			for _, record := range pending {
				if err := reopenedPending.Insert(ctx, record); err != nil {
					t.Fatal(err)
				}
			}
			f.reopen(t, reopenedPending)
			if _, err := f.store.Append(ctx, "recover-large", session.AnySequence, largePending(401<<10)); !errors.Is(err, ErrPending) {
				t.Fatalf("changed request replaced original: %v", err)
			}
			recovered, err := f.store.Append(ctx, "recover-large", session.AnySequence, input...)
			if mode == "before" || mode == "upload-before" {
				if !errors.Is(err, aterequest.ErrUnknown) || recovered != nil {
					t.Fatalf("ambiguous absence resubmitted: %v", err)
				}
				for _, originalRecord := range pending {
					record, found, err := f.pending.Get(ctx, originalRecord.Request.Key())
					if err != nil || !found || !aterequest.Equal(record, originalRecord) || record.Attempted != originalRecord.Attempted {
						t.Fatalf("original pending identity changed: %v", err)
					}
				}
				return
			}
			if err != nil || !reflect.DeepEqual(original, recovered) {
				t.Fatalf("recovery changed identities: %v", err)
			}
			loaded, err := f.store.Load(ctx, "recover-large", 0, 10)
			if err != nil || !reflect.DeepEqual(original, loaded) || len(f.pending.rows) != 0 {
				t.Fatalf("recovery lost/duplicated events or retained history: %v", err)
			}
		})
	}
}

type failAppendInsert struct {
	*pendingStore
	failed bool
}

func (p *failAppendInsert) Insert(ctx context.Context, record aterequest.Record) error {
	if !p.failed && strings.HasPrefix(string(record.Request.RequestID), requestPrefix+"append-") {
		p.failed = true
		return errors.New("injected failure saving final append")
	}
	return p.pendingStore.Insert(ctx, record)
}

func TestNativeRecoveryBetweenPendingInserts(t *testing.T) {
	f := newNativeFixture(t)
	ctx := t.Context()
	store, err := New(Options{Process: f.process, Pending: &failAppendInsert{pendingStore: f.pending}, Space: f.store.space, StoreID: f.storeID})
	if err != nil {
		t.Fatal(err)
	}
	input := []session.PendingEvent{largePending(500 << 10), pendingEvent("same-batch")}
	if _, err := store.Append(ctx, "save-gap", 0, input...); err == nil {
		t.Fatal("missing injection")
	}
	pending, err := f.pending.List(ctx, aterequest.ListOptions{Limit: 128, MaxBytes: aterequest.MaxListBytes})
	if err != nil || len(pending) != 1 || pending[0].Attempted {
		t.Fatalf("upload attempted before final request saved: %v", err)
	}
	_, original, err := f.store.uploadPlan(pending[0].Request)
	if err != nil {
		t.Fatal(err)
	}
	recoveredPending := newPending()
	if err := recoveredPending.Insert(ctx, pending[0]); err != nil {
		t.Fatal(err)
	}
	f.reopen(t, recoveredPending)
	events, err := f.store.Append(ctx, "save-gap", 0, input...)
	if err != nil || !reflect.DeepEqual(original, events) {
		t.Fatalf("partial preparation lost identity: %v", err)
	}
}

func TestNativeLargeReadBudgetIsCompleteOrError(t *testing.T) {
	f := newNativeFixture(t)
	ctx := t.Context()
	for i := 0; i < 3; i++ {
		if _, err := f.store.Append(ctx, "read-budget", uint64(i), largePending(MaxAppendBytes-2000)); err != nil {
			nativeFailure(t, err)
		}
	}
	if events, err := f.store.Load(ctx, "read-budget", 0, 10); !errors.Is(err, ErrLimit) || events != nil {
		t.Fatalf("read exceeded expanded-byte budget: %v", err)
	}
	if records, err := f.store.HistoryThrough(ctx, "read-budget", 0, 3, 10); !errors.Is(err, ErrLimit) || records != nil {
		t.Fatalf("history exposed partial cut: %v", err)
	}
	if events, err := f.store.Load(ctx, "read-budget", 1, 2); err != nil || len(events) != 2 {
		t.Fatalf("bounded page could not progress: %v", err)
	}
}

type payloadReadFunc func(context.Context, api.SessionEventItem, io.Writer) error

func (fn payloadReadFunc) ReadSessionEventPayload(ctx context.Context, item api.SessionEventItem, w io.Writer) error {
	return fn(ctx, item, w)
}

type fixedViewReader struct {
	NativeReader
	view     api.ViewDescriptor
	mode     string
	cancel   context.CancelFunc
	closed   bool
	anchored bool
}

func (r *fixedViewReader) OpenView(context.Context, api.OpenViewRequest) (api.ViewDescriptor, error) {
	return r.view, nil
}

func (r *fixedViewReader) GetSession(_ context.Context, request api.ReadSessionRequest) (api.SessionResult, error) {
	r.anchored = request.Anchor != nil && *request.Anchor == r.view.Anchor
	if r.mode == "cancel" {
		r.cancel()
		return api.SessionResult{}, context.Canceled
	}
	anchor := r.view.Anchor
	if r.mode == "drift" {
		anchor.Fence.LSN++
	}
	return api.SessionResult{Anchor: anchor}, nil
}

func (r *fixedViewReader) CloseView(ctx context.Context, request api.CloseViewRequest) error {
	r.closed = request.Ref == r.view.Ref && ctx.Err() == nil
	if _, bounded := ctx.Deadline(); !bounded {
		return errors.New("view cleanup has no deadline")
	}
	if r.mode == "close" {
		return errors.New("injected view cleanup failure")
	}
	return ctx.Err()
}

func TestSessionReadPinsAnchorAndClosesOnEveryOutcome(t *testing.T) {
	for _, mode := range []string{"empty", "drift", "cancel", "close"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			reader := &fixedViewReader{mode: mode, cancel: cancel, view: api.ViewDescriptor{
				Anchor: model.SnapshotAnchor{Space: "sessions", Fence: model.CommitFence{
					Store: "native", CommitDigest: model.CommitDigest(strings.Repeat("0", 64)),
				}},
			}}
			store := &Store{space: "sessions", storeID: "native"}
			items, err := store.readNative(ctx, reader, "session", 0, nil, nil, 1)
			if !reader.anchored || !reader.closed {
				t.Fatalf("read did not retain/release its fixed view: %+v", reader)
			}
			if mode == "empty" {
				if err != nil || len(items) != 0 {
					t.Fatalf("empty anchored history: %v", err)
				}
			} else if err == nil || items != nil {
				t.Fatalf("failed read exposed results: %v", err)
			}
			if mode == "drift" && !errors.Is(err, ErrIdentity) {
				t.Fatalf("anchor drift accepted: %v", err)
			}
		})
	}
}

func TestReferencedPayloadChecksBudgetLengthAndDigest(t *testing.T) {
	for _, mode := range []string{"budget", "overflow", "short", "digest"} {
		t.Run(mode, func(t *testing.T) {
			item := api.SessionEventItem{Event: agentprofile.SessionEvent{Event: agentprofile.HostEvent{
				PayloadRef: agentprofile.HostPayloadRef{Bytes: 3, Digest: model.ObjectID(strings.Repeat("a", 64)),
					Chunks: []agentprofile.HostPayloadChunk{{Key: []byte("key"), Object: model.ObjectID(strings.Repeat("b", 64))}}},
			}}}
			calls := 0
			reader := payloadReadFunc(func(_ context.Context, _ api.SessionEventItem, writer io.Writer) error {
				calls++
				data := "bad"
				if mode == "overflow" {
					data = "too-long"
				}
				if mode == "short" {
					data = "x"
				}
				_, err := io.WriteString(writer, data)
				return err
			})
			budget := 3
			if mode == "budget" {
				budget = 2
			}
			if _, err := resolveItem(t.Context(), reader, item, budget); err == nil {
				t.Fatal("unverified payload accepted")
			}
			if mode == "budget" && calls != 0 {
				t.Fatal("read started before checking manifest size")
			}
		})
	}
}

func TestNativeAggregateBatchPublishesInOneAppend(t *testing.T) {
	f := newNativeFixture(t)
	ctx := t.Context()
	var before model.CommitFence
	if err := f.process.WithClient(ctx, func(c *client.Client) error {
		snapshot, err := c.Snapshot(ctx)
		before = snapshot.Fence
		return err
	}); err != nil {
		t.Fatal(err)
	}
	input := make([]session.PendingEvent, 6)
	for i := range input {
		input[i] = largePending(200 << 10)
	}
	events, err := f.store.Append(ctx, "aggregate", 0, input...)
	if err != nil {
		nativeFailure(t, err)
	}
	if err := f.process.WithClient(ctx, func(c *client.Client) error {
		snapshot, err := c.Snapshot(ctx)
		if err != nil {
			return err
		}
		if snapshot.Fence.LSN-before.LSN != 3 {
			return errors.New("logical append split across extra commits")
		}
		page, err := c.ListSessionEvents(ctx, api.ListSessionEventsRequest{
			Space: f.store.space, Principal: "gsea", SessionID: "aggregate", Limit: 64,
		})
		if err != nil {
			return err
		}
		if len(page.Items) != 6 {
			return errors.New("batch partially published")
		}
		for _, item := range page.Items {
			if !item.Event.Event.PayloadRef.Present() {
				return errors.New("oversize aggregate left inline")
			}
		}
		return nil
	}); err != nil {
		nativeFailure(t, err)
	}
	loaded, err := f.store.Load(ctx, "aggregate", 0, 10)
	if err != nil || !reflect.DeepEqual(events, loaded) {
		t.Fatalf("aggregate round trip: %v", err)
	}
}

func TestNativeRejectedUploadCannotPublishDependentAppend(t *testing.T) {
	f := newNativeFixture(t)
	ctx := t.Context()
	t.Setenv(dropEnvironment, "upload-rejected")
	input := largePending(300 << 10)
	if _, err := f.store.Append(ctx, "rejected-upload", 0, input); !errors.Is(err, aterequest.ErrRejected) {
		t.Fatalf("conclusive upload rejection lost: %v", err)
	}
	t.Setenv(dropEnvironment, "")
	if events, err := f.store.Append(ctx, "rejected-upload", 0, input); !errors.Is(err, ErrPending) || events != nil {
		t.Fatalf("dependent append submitted without native upload: %v", err)
	}
	if sequence, err := f.store.CurrentSequence(ctx, "rejected-upload"); err != nil || sequence != 0 {
		t.Fatalf("rejected upload published history: %d %v", sequence, err)
	}
	records, err := f.pending.List(ctx, aterequest.ListOptions{Limit: 128, MaxBytes: aterequest.MaxListBytes})
	if err != nil || len(records) != 1 || records[0].Attempted {
		t.Fatalf("dependent append was discarded or submitted: %v", err)
	}
}

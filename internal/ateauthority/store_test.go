package ateauthority

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ate "github.com/vkviyu/agent-temporal-engine"
	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/agent-temporal-engine/service"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateprocess"
	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
)

var _ Process = (*ateprocess.Manager)(nil)

// The test pending store contains only canonical replay envelopes, never JSON
// business projections. Reloading it exercises the same encode/decode boundary
// required of a durable host PendingStore.
type pendingMemory struct {
	mu                sync.Mutex
	rows              map[api.RequestKey][]byte
	inserts           int
	failCleanup       bool
	failRejectCleanup bool
	attempted         map[api.RequestKey]bool
	rejected          map[api.RequestKey]api.CommitResult
}

func (p *pendingMemory) Insert(_ context.Context, record aterequest.Record) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	raw, err := aterequest.EncodeRecord(record)
	if err != nil {
		return err
	}
	key := record.Request.Key()
	if _, rejected := p.rejected[key]; rejected {
		return aterequest.ErrConflict
	}
	if old, ok := p.rows[key]; ok && !bytes.Equal(old, raw) {
		return aterequest.ErrConflict
	}
	p.rows[key] = raw
	p.inserts++
	return nil
}

func (p *pendingMemory) Get(_ context.Context, key api.RequestKey) (aterequest.Record, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	raw, ok := p.rows[key]
	if !ok {
		return aterequest.Record{}, false, nil
	}
	record, err := aterequest.DecodeRecord(raw)
	record.Attempted = p.attempted[key]
	return record, true, err
}

func (p *pendingMemory) BeginAttempt(_ context.Context, record aterequest.Record) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := record.Request.Key()
	if p.attempted[key] {
		return false, nil
	}
	stored, err := aterequest.DecodeRecord(p.rows[key])
	if err != nil || !aterequest.Equal(stored, record) {
		return false, aterequest.ErrConflict
	}
	if p.attempted == nil {
		p.attempted = make(map[api.RequestKey]bool)
	}
	p.attempted[key] = true
	return true, nil
}

func (p *pendingMemory) DeleteRejected(_ context.Context, record aterequest.Record, result api.CommitResult) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := aterequest.VerifyRejection(record, result); err != nil {
		return err
	}
	if p.failRejectCleanup {
		return errors.New("injected rejection cleanup failure")
	}
	key := record.Request.Key()
	stored, err := aterequest.DecodeRecord(p.rows[key])
	if err != nil || !aterequest.Equal(stored, record) {
		return aterequest.ErrConflict
	}
	if p.rejected == nil {
		p.rejected = make(map[api.RequestKey]api.CommitResult)
	}
	p.rejected[key] = result
	delete(p.rows, key)
	return nil
}

func (p *pendingMemory) List(_ context.Context, options aterequest.ListOptions) ([]aterequest.Record, error) {
	if err := options.Validate(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var records []aterequest.Record
	for _, raw := range p.rows {
		r, err := aterequest.DecodeRecord(raw)
		if err != nil {
			return nil, err
		}
		r.Attempted = p.attempted[r.Request.Key()]
		records = append(records, r)
	}
	compare := func(a, b api.RequestKey) int {
		for _, pair := range [][2]string{{string(a.Space), string(b.Space)}, {string(a.Principal), string(b.Principal)}, {string(a.RequestID), string(b.RequestID)}} {
			if pair[0] < pair[1] {
				return -1
			}
			if pair[0] > pair[1] {
				return 1
			}
		}
		return 0
	}
	slices.SortFunc(records, func(a, b aterequest.Record) int { return compare(a.Request.Key(), b.Request.Key()) })
	var out []aterequest.Record
	used := 0
	for _, r := range records {
		if options.After != nil && compare(r.Request.Key(), *options.After) <= 0 {
			continue
		}
		raw, _ := aterequest.EncodeRecord(r)
		if len(out) == options.Limit || used+len(raw) > options.MaxBytes {
			break
		}
		used += len(raw)
		out = append(out, r)
	}
	return out, nil
}

func (p *pendingMemory) DeleteAcknowledged(_ context.Context, record aterequest.Record, result api.CommitResult) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := aterequest.VerifyAcknowledgement(record, result); err != nil {
		return err
	}
	if p.failCleanup {
		return errors.New("injected cleanup failure")
	}
	raw, ok := p.rows[record.Request.Key()]
	if !ok {
		return nil
	}
	stored, err := aterequest.DecodeRecord(raw)
	if err != nil {
		return err
	}
	if !aterequest.Equal(stored, record) {
		return aterequest.ErrConflict
	}
	delete(p.rows, record.Request.Key())
	return nil
}

// Faults are injected around a real durable engine, never replacing its CAS or
// request receipts. Every Store call still uses the real native client/server.
type backend struct {
	*ate.Store
	commits       atomic.Int32
	reads         atomic.Int32
	loseReply     atomic.Bool
	failLookup    atomic.Bool
	conflict      atomic.Bool
	idempotency   atomic.Bool
	reject        atomic.Bool
	unknownBefore atomic.Bool
}

func (b *backend) Commit(ctx context.Context, r api.CommitRequest) (api.CommitResult, error) {
	b.commits.Add(1)
	if r.Method != api.MethodMutateState || r.ExpectedFence != nil {
		return api.CommitResult{}, errors.New("non-native mutation or global fence used")
	}
	if b.conflict.Load() || b.idempotency.Load() {
		reason := api.ConflictPrecondition
		if b.idempotency.Load() {
			reason = api.ConflictIdempotency
		}
		return api.CommitResult{Code: api.CodeConflict, Conflict: reason, RequestID: r.RequestID}, nil
	}
	if b.reject.Load() {
		return api.CommitResult{Code: api.CodeRejected, RequestID: r.RequestID}, nil
	}
	if b.unknownBefore.Load() {
		b.failLookup.Store(true)
		return api.CommitResult{}, errors.New("lost request before commit")
	}
	result, err := b.Store.Commit(ctx, r)
	if err == nil && result.Code == api.CodeDurableAck && b.loseReply.Load() {
		b.failLookup.Store(true)
		return result, errors.New("lost durable response")
	}
	return result, err
}

func (b *backend) LookupRequest(ctx context.Context, r api.LookupRequestRequest) (api.LookupRequestResult, error) {
	if b.failLookup.Load() {
		return api.LookupRequestResult{}, errors.New("lookup unavailable")
	}
	return b.Store.LookupRequest(ctx, r)
}

func (b *backend) GetViewState(ctx context.Context, r api.ReadViewStateRequest) (api.StateValueResult, error) {
	b.reads.Add(1)
	return b.Store.GetViewState(ctx, r)
}

type nativeProcess struct{ backend *backend }

func (p *nativeProcess) WithClient(ctx context.Context, fn func(*client.Client) error) error {
	server, err := service.New(p.backend, service.Options{Credentials: map[string]service.Credential{
		"test": {Principal: "gsea", AllSpaces: true},
	}})
	if err != nil {
		return err
	}
	defer server.Shutdown(context.Background())
	a, b := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- server.ServeStream(ctx, b) }()
	defer func() { _ = a.Close(); <-done }()
	c, err := client.New(ctx, a, client.Options{Token: "test"})
	if err != nil {
		return err
	}
	defer c.Close()
	return fn(c)
}

type fixture struct {
	t       *testing.T
	parent  string
	path    string
	backend *backend
	process *nativeProcess
	pending *pendingMemory
	store   *Store
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	parent := t.TempDir()
	path := filepath.Join(parent, "native")
	must(t, os.Mkdir(path, 0700))
	engine, err := ate.Create(path, ate.Options{})
	must(t, err)
	f := &fixture{t: t, parent: parent, path: path, backend: &backend{Store: engine},
		pending: &pendingMemory{rows: make(map[api.RequestKey][]byte)}}
	f.start()
	t.Cleanup(func() {
		must(t, f.backend.Store.Close())
	})
	return f
}

func (f *fixture) start() {
	var err error
	f.process = &nativeProcess{backend: f.backend}
	f.store, err = New(Options{Process: f.process, Pending: f.pending, Space: "authority", Principal: "gsea"})
	must(f.t, err)
}

func (f *fixture) reopen() {
	must(f.t, f.backend.Store.Close())
	store, report, err := ate.OpenExisting(f.path, ate.Options{})
	must(f.t, err)
	if !report.Verified || !report.PublicationSynced {
		f.t.Fatal("reopen did not verify native authority")
	}
	f.backend.Store = store
	f.start()
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func readInt(t *testing.T, s *Store, key string) int {
	t.Helper()
	var value int
	must(t, s.View(context.Background(), func(tx *Tx) error {
		found, err := tx.Get(key, &value)
		if err == nil && !found {
			return fmt.Errorf("missing %s", key)
		}
		return err
	}))
	return value
}

func TestRecordsOverlayReopenAndNoLocalBusinessWrites(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var escaped *Tx
	must(t, f.store.Update(ctx, func(tx *Tx) error {
		escaped = tx
		if tx.Context().Err() != nil {
			t.Fatal("transaction context unexpectedly canceled")
		}
		must(t, tx.Create("item/z", 1))
		if err := tx.Create("item/z", 2); !errors.Is(err, ErrExists) {
			t.Fatalf("duplicate create: %v", err)
		}
		must(t, tx.Put("item/a", map[string]int{"n": 2}))
		var got map[string]int
		found, err := tx.Get("item/a", &got)
		if err != nil || !found || got["n"] != 2 {
			t.Fatalf("read own write: %v %v %v", found, got, err)
		}
		must(t, tx.Delete("item/z"))
		must(t, tx.Create("item/z", 3))
		must(t, tx.Put("item/null", nil))
		must(t, tx.Put("other", 4))
		var keys []string
		must(t, tx.List("item/", func(key string, raw json.RawMessage) error {
			keys = append(keys, key)
			raw[0] = '!' // visitor owns bytes, including staged values
			return nil
		}))
		if !slices.Equal(keys, []string{"item/a", "item/null", "item/z"}) {
			t.Fatalf("unstable list: %v", keys)
		}
		return nil
	}))
	if err := escaped.Put("escape", 1); !errors.Is(err, ErrClosed) {
		t.Fatalf("escaped transaction: %v", err)
	}
	if len(f.pending.rows) != 0 || f.pending.inserts != 1 {
		t.Fatalf("pending not acknowledged: %d rows, %d inserts", len(f.pending.rows), f.pending.inserts)
	}
	f.reopen()
	if got := readInt(t, f.store, "item/z"); got != 3 {
		t.Fatalf("reopen value %d", got)
	}
	must(t, f.store.View(ctx, func(tx *Tx) error {
		var absent = 9
		if found, err := tx.Get("absent", &absent); found || err != nil || absent != 9 {
			t.Fatalf("absence changed output: %v %v %d", found, err, absent)
		}
		var null any = 1
		if found, err := tx.Get("item/null", &null); !found || err != nil || null != nil {
			t.Fatalf("null confused with absence: %v %v %v", found, err, null)
		}
		for _, err := range []error{tx.Put("bad", 1), tx.Create("bad", 1), tx.Delete("item/z")} {
			if !errors.Is(err, ErrReadOnly) {
				t.Fatalf("read-only mutation: %v", err)
			}
		}
		before := f.backend.reads.Load()
		var n int
		_, err := tx.Get("item/z", &n)
		must(t, err)
		_, err = tx.Get("item/z", &n)
		must(t, err)
		if f.backend.reads.Load()-before != 2 {
			t.Fatal("reads used a local projection")
		}
		return nil
	}))
	// Inspect ATE itself: business entities are independent JSON byte records.
	for key, want := range map[string]string{"item/z": "3", "item/null": "null", "other": "4"} {
		result, err := f.backend.Store.GetState(ctx, api.ReadStateRequest{Space: "authority", Key: []byte(key)})
		must(t, err)
		if !result.Found || result.Value.Kind != api.ValueBytes || string(result.Value.Data) != want {
			t.Fatalf("not an individual ATE record: %+v", result)
		}
	}
	files, err := os.ReadDir(f.parent)
	must(t, err)
	if len(files) != 1 || files[0].Name() != "native" {
		t.Fatalf("host business files created: %v", files)
	}
}

func TestAbortEmptyUpdateAndVisitorError(t *testing.T) {
	f := newFixture(t)
	sentinel := errors.New("callback abort")
	err := f.store.Update(context.Background(), func(tx *Tx) error {
		must(t, tx.Put("never", 1))
		return sentinel
	})
	if !errors.Is(err, sentinel) || f.backend.commits.Load() != 0 || f.pending.inserts != 0 {
		t.Fatalf("aborted callback submitted: %v", err)
	}
	must(t, f.store.Update(context.Background(), func(tx *Tx) error { return nil }))
	if f.backend.commits.Load() != 0 {
		t.Fatal("empty update committed")
	}
	must(t, f.store.Update(context.Background(), func(tx *Tx) error { return tx.Put("one", 1) }))
	err = f.store.Update(context.Background(), func(tx *Tx) error {
		must(t, tx.Put("two", 2))
		return tx.List("", func(string, json.RawMessage) error { return sentinel })
	})
	if !errors.Is(err, sentinel) || f.backend.commits.Load() != 1 {
		t.Fatalf("visitor failure submitted: %v", err)
	}
}

func TestConcurrentCASAndPhantomProtection(t *testing.T) {
	for _, phantom := range []bool{false, true} {
		t.Run(fmt.Sprint("phantom=", phantom), func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			must(t, f.store.Update(ctx, func(tx *Tx) error { return tx.Put("count", 0) }))
			ready := make(chan struct{}, 2)
			release := make(chan struct{})
			out := make(chan error, 2)
			var callbacks atomic.Int32
			for worker := 0; worker < 2; worker++ {
				go func() {
					first := true
					out <- f.store.Update(ctx, func(tx *Tx) error {
						callbacks.Add(1)
						var n int
						if phantom {
							if err := tx.List("claim/", func(string, json.RawMessage) error { n++; return nil }); err != nil {
								return err
							}
						} else if _, err := tx.Get("count", &n); err != nil {
							return err
						}
						if first {
							first = false
							ready <- struct{}{}
							<-release
						}
						if phantom {
							if n != 0 {
								return nil
							}
							return tx.Create(fmt.Sprintf("claim/%d", worker), worker)
						}
						return tx.Put("count", n+1)
					})
				}()
			}
			<-ready
			<-ready
			close(release)
			must(t, <-out)
			must(t, <-out)
			if callbacks.Load() != 3 {
				t.Fatalf("expected one proven conflict retry, callbacks=%d", callbacks.Load())
			}
			if !phantom && readInt(t, f.store, "count") != 2 {
				t.Fatal("lost concurrent increment")
			}
			if phantom {
				n := 0
				must(t, f.store.View(ctx, func(tx *Tx) error {
					return tx.List("claim/", func(string, json.RawMessage) error { n++; return nil })
				}))
				if n != 1 {
					t.Fatalf("phantom allowed two claims: %d", n)
				}
			}
		})
	}
}

func TestAnchoredReadsAndUnrelatedSpaces(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	must(t, f.store.Update(ctx, func(tx *Tx) error { return tx.Put("value", 1) }))
	must(t, f.store.View(ctx, func(tx *Tx) error {
		var first, second int
		_, err := tx.Get("value", &first)
		must(t, err)
		must(t, f.store.Update(ctx, func(other *Tx) error { return other.Put("value", 2) }))
		_, err = tx.Get("value", &second)
		must(t, err)
		if first != 1 || second != 1 {
			t.Fatalf("view drifted: %d %d", first, second)
		}
		return nil
	}))
	if readInt(t, f.store, "value") != 2 {
		t.Fatal("subsequent view was stale")
	}
	other, err := New(Options{Process: f.process, Pending: f.pending, Space: "unrelated", Principal: "gsea"})
	must(t, err)
	calls := 0
	must(t, f.store.Update(ctx, func(tx *Tx) error {
		calls++
		must(t, other.Update(ctx, func(otherTx *Tx) error { return otherTx.Put("x", calls) }))
		return tx.Put("value", 3)
	}))
	if calls != 1 {
		t.Fatal("used global fence instead of authority-space root CAS")
	}
}

func TestRetryClassification(t *testing.T) {
	for _, mode := range []string{"precondition", "idempotency", "rejected", "callback-conflict"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.backend.conflict.Store(mode == "precondition")
			f.backend.idempotency.Store(mode == "idempotency")
			f.backend.reject.Store(mode == "rejected")
			calls := 0
			err := f.store.Update(context.Background(), func(tx *Tx) error {
				calls++
				if mode == "callback-conflict" {
					return ErrConflict
				}
				return tx.Put("x", 1)
			})
			wantCalls := 1
			wantError := aterequest.ErrRejected
			switch mode {
			case "precondition":
				wantCalls, wantError = MaxUpdateAttempts, ErrConflict
			case "idempotency":
				wantError = aterequest.ErrConflict
			case "callback-conflict":
				wantError = ErrConflict
			}
			if calls != wantCalls || !errors.Is(err, wantError) || errors.Is(err, aterequest.ErrUnknown) {
				t.Fatalf("incorrect retry/error mapping: calls=%d err=%v", calls, err)
			}
			if mode != "callback-conflict" {
				var requestErr *RequestError
				if !errors.As(err, &requestErr) || requestErr.Key.RequestID == "" || requestErr.StoreID == "" {
					t.Fatalf("missing request evidence: %v", err)
				}
				if len(f.pending.rows) != 0 || len(f.pending.rejected) != wantCalls {
					t.Fatal("rejection not retired with terminal identity fence")
				}
			}
		})
	}
}

func TestUnknownRecoveryAndCleanupAcrossReopen(t *testing.T) {
	for _, mode := range []string{"durable-lost-reply", "absent-lost-request", "unattempted", "cleanup"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.backend.loseReply.Store(mode == "durable-lost-reply")
			f.backend.unknownBefore.Store(mode == "absent-lost-request")
			f.backend.failLookup.Store(mode == "unattempted")
			f.pending.failCleanup = mode == "cleanup"
			calls := 0
			err := f.store.Update(context.Background(), func(tx *Tx) error {
				calls++
				return tx.Create("once", 1)
			})
			want := aterequest.ErrUnknown
			if mode == "cleanup" {
				want = aterequest.ErrCleanup
			}
			var requestErr *RequestError
			if !errors.Is(err, want) || !errors.As(err, &requestErr) || calls != 1 {
				t.Fatalf("unknown/cleanup recomputed: calls=%d err=%v", calls, err)
			}
			record, found, err := f.pending.Get(context.Background(), requestErr.Key)
			must(t, err)
			if !found || record.StoreID != requestErr.StoreID {
				t.Fatal("original replay envelope missing")
			}
			raw := bytes.Clone(f.pending.rows[requestErr.Key])
			// Recreate both the authority Store and the pending implementation
			// using only persisted replay bytes; no callback state survives.
			f.pending = &pendingMemory{rows: map[api.RequestKey][]byte{requestErr.Key: raw},
				attempted: map[api.RequestKey]bool{requestErr.Key: record.Attempted}}
			f.backend.loseReply.Store(false)
			f.backend.unknownBefore.Store(false)
			f.backend.failLookup.Store(false)
			f.reopen()
			before := f.backend.commits.Load()
			err = f.store.Resume(context.Background(), requestErr.Key)
			if mode == "absent-lost-request" {
				if !errors.Is(err, aterequest.ErrUnknown) || f.backend.commits.Load() != before ||
					!bytes.Equal(f.pending.rows[requestErr.Key], raw) {
					t.Fatalf("ambiguous absence replayed or bytes lost: %v", err)
				}
				return
			}
			must(t, err)
			wantCommits := before
			if mode == "unattempted" {
				wantCommits++
			}
			if f.backend.commits.Load() != wantCommits || calls != 1 || len(f.pending.rows) != 0 {
				t.Fatal("recovery recomputed or duplicated acknowledged command")
			}
			if readInt(t, f.store, "once") != 1 {
				t.Fatal("recovered wrong business record")
			}
			result, err := f.backend.Store.LookupRequest(context.Background(), api.LookupRequestRequest{
				Space: record.Request.Space, Principal: record.Request.Principal, RequestID: record.Request.RequestID,
			})
			must(t, err)
			must(t, aterequest.VerifyAcknowledgement(record, result.Result))
		})
	}
}

func TestRecoveryRejectsDifferentNativeStore(t *testing.T) {
	f := newFixture(t)
	f.backend.unknownBefore.Store(true)
	err := f.store.Update(context.Background(), func(tx *Tx) error { return tx.Put("x", 1) })
	var requestErr *RequestError
	if !errors.As(err, &requestErr) {
		t.Fatal(err)
	}
	other := newFixture(t)
	other.store.options.Pending = f.pending
	err = other.store.Resume(context.Background(), requestErr.Key)
	if !errors.Is(err, ErrIdentity) || other.backend.commits.Load() != 0 {
		t.Fatalf("replayed at replacement authority: %v", err)
	}
}

func TestListOverlaySnapshotAndLimits(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	must(t, f.store.Update(ctx, func(tx *Tx) error {
		must(t, tx.Put("a", 1))
		must(t, tx.Put("b", 2))
		return nil
	}))
	must(t, f.store.Update(ctx, func(tx *Tx) error {
		must(t, tx.Put("c", 3))
		var keys []string
		must(t, tx.List("", func(key string, raw json.RawMessage) error {
			keys = append(keys, key)
			if key == "a" {
				must(t, tx.Delete("b"))
				must(t, tx.Put("c", 30))
			}
			if key == "c" && string(raw) != "3" {
				t.Fatal("visitor changed in-progress overlay snapshot")
			}
			return nil
		}))
		if !slices.Equal(keys, []string{"a", "b", "c"}) {
			t.Fatalf("visitor changed enumeration: %v", keys)
		}
		if err := tx.Put("oversize", string(make([]byte, api.MaxStateValueBytes))); !errors.Is(err, ErrLimit) {
			t.Fatalf("unbounded value: %v", err)
		}
		return nil
	}))
	if readInt(t, f.store, "c") != 30 {
		t.Fatal("visitor writes lost")
	}
	// Populate beyond the old whole-space ceiling. Both narrow prefixes and
	// matching enumerations must work through bounded native pages.
	const historyRecords = 5 * api.MaxStateMutations
	for start := 0; start < historyRecords; start += api.MaxStateMutations {
		ops := make([]api.StateMutation, api.MaxStateMutations)
		for i := range ops {
			ops[i] = api.StateMutation{Key: []byte(fmt.Sprintf("bulk/%05d", start+i)), Kind: api.MutationPut,
				Value: api.Value{Kind: api.ValueBytes, Data: []byte("0")}}
		}
		result, err := f.backend.Store.Mutate(ctx, api.MutateRequest{
			Key:   model.RequestKey{Space: "authority", Principal: "gsea", RequestID: model.RequestID(fmt.Sprintf("seed-%d", start))},
			Batch: api.StateBatch{Operations: ops},
		})
		must(t, err)
		if result.Code != api.CodeDurableAck {
			t.Fatalf("seed rejected: %+v", result)
		}
	}
	visits := 0
	err := f.store.View(ctx, func(tx *Tx) error {
		return tx.List("nonmatching/", func(string, json.RawMessage) error { visits++; return nil })
	})
	if err != nil || visits != 0 {
		t.Fatalf("unrelated catalog affected prefix: visits=%d err=%v", visits, err)
	}
	visits = 0
	must(t, f.store.Update(ctx, func(tx *Tx) error {
		must(t, tx.Delete("bulk/00255"))
		must(t, tx.Put("bulk/00255-new", 7))
		must(t, tx.Put("bulk/00256", 8))
		must(t, tx.Put("bulk/zz", 9))
		previous := ""
		return tx.List("bulk/", func(key string, raw json.RawMessage) error {
			if key <= previous || key == "bulk/00255" {
				return fmt.Errorf("bad merge order at %s after %s", key, previous)
			}
			if key == "bulk/00256" && string(raw) != "8" {
				return errors.New("staged page-boundary overwrite lost")
			}
			previous = key
			visits++
			return nil
		})
	}))
	if visits != historyRecords+1 {
		t.Fatalf("paged list truncated/duplicated: %d", visits)
	}
}

func TestNativeSubprocessManager(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("test source location unavailable")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", "..", "..", "agent-temporal-engine"))
	parent := t.TempDir()
	binary := filepath.Join(parent, "ate")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-o", binary, "./cmd/ate")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build native ATE: %v\n%s", err, output)
	}
	pending := &pendingMemory{rows: make(map[api.RequestKey][]byte)}
	directory := filepath.Join(parent, "authority")
	run := func(write bool) {
		manager, err := ateprocess.New(ateprocess.Options{Binary: binary, Directory: directory, Timeout: 15 * time.Second})
		must(t, err)
		defer manager.Close()
		store, err := New(Options{Process: manager, Pending: pending, Space: "orchestration-authority", Principal: "gsea"})
		must(t, err)
		if write {
			must(t, store.Update(ctx, func(tx *Tx) error { return tx.Create("task/one", 42) }))
		} else if readInt(t, store, "task/one") != 42 {
			t.Fatal("new process did not reopen committed authority")
		}
	}
	run(true)
	run(false)
	if pending.inserts != 1 || len(pending.rows) != 0 {
		t.Fatal("subprocess commit was not durably acknowledged and cleaned")
	}
	for _, writable := range []bool{false, true} {
		t.Run(fmt.Sprint("timeout-write=", writable), func(t *testing.T) {
			manager, err := ateprocess.New(ateprocess.Options{Binary: binary, Directory: directory, Timeout: time.Second})
			must(t, err)
			defer manager.Close()
			managerReturned := make(chan struct{})
			process := processFunc(func(ctx context.Context, fn func(*client.Client) error) error {
				err := manager.WithClient(ctx, fn)
				close(managerReturned)
				return err
			})
			store, err := New(Options{Process: process, Pending: pending, Space: "orchestration-authority", Principal: "gsea"})
			must(t, err)
			started, release := make(chan struct{}), make(chan struct{})
			returned := make(chan error, 1)
			result := 0
			go func() {
				callback := func(tx *Tx) error {
					close(started)
					<-release
					result = 123
					if writable {
						return tx.Put("late", 1)
					}
					return nil
				}
				if writable {
					returned <- store.Update(ctx, callback)
				} else {
					returned <- store.View(ctx, callback)
				}
			}()
			select {
			case <-started:
			case err := <-returned:
				t.Fatalf("callback never started: %v", err)
			}
			<-managerReturned
			select {
			case err := <-returned:
				t.Fatalf("Store returned before caller result mutation ended: %v", err)
			default:
			}
			close(release)
			if err := <-returned; err == nil {
				t.Fatal("timeout reported success")
			}
			if result != 123 {
				t.Fatal("callback not joined")
			}
		})
	}
}

func TestRejectionCleanupStopsCallbackRetry(t *testing.T) {
	f := newFixture(t)
	f.backend.conflict.Store(true)
	f.pending.failRejectCleanup = true
	calls := 0
	err := f.store.Update(context.Background(), func(tx *Tx) error {
		calls++
		return tx.Put("x", 1)
	})
	var requestErr *RequestError
	if calls != 1 || !errors.Is(err, ErrConflict) || !errors.Is(err, aterequest.ErrRejectionCleanup) ||
		!errors.As(err, &requestErr) {
		t.Fatalf("cleanup error hidden by retry: calls=%d err=%v", calls, err)
	}
	record, found, err := f.pending.Get(context.Background(), requestErr.Key)
	must(t, err)
	if !found || !record.Attempted {
		t.Fatal("rejected request lost its attempt fence")
	}
	// The exact known rejection can finish cleanup without submitting again.
	f.pending.failRejectCleanup = false
	must(t, f.pending.DeleteRejected(context.Background(), record, requestErr.Result))
	if err := f.pending.Insert(context.Background(), record); !errors.Is(err, aterequest.ErrConflict) {
		t.Fatalf("terminal rejected identity became reusable: %v", err)
	}
}

func TestValidationCancellationAndCorruptValues(t *testing.T) {
	f := newFixture(t)
	for _, options := range []Options{
		{}, {Process: f.process, Pending: f.pending, Principal: "gsea"},
		{Process: f.process, Pending: f.pending, Space: "ate:reserved", Principal: "gsea"},
	} {
		if _, err := New(options); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid configuration accepted: %v", err)
		}
	}
	if err := f.store.View(nil, func(*Tx) error { return nil }); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil context accepted: %v", err)
	}
	if err := f.store.Update(context.Background(), nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil callback accepted: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	err := f.store.Update(ctx, func(*Tx) error { called = true; return nil })
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("canceled operation invoked callback: %v", err)
	}
	wrong, err := New(Options{Process: f.process, Pending: f.pending, Space: "authority", Principal: "other"})
	must(t, err)
	err = wrong.Update(context.Background(), func(*Tx) error { called = true; return nil })
	if !errors.Is(err, ErrIdentity) || called {
		t.Fatalf("principal mismatch invoked callback: %v", err)
	}
	result, err := f.backend.Store.Mutate(context.Background(), api.MutateRequest{
		Key: model.RequestKey{Space: "authority", Principal: "gsea", RequestID: "corrupt-json"},
		Batch: api.StateBatch{Operations: []api.StateMutation{{
			Key: []byte("broken"), Kind: api.MutationPut, Value: api.Value{Kind: api.ValueBytes, Data: []byte("not JSON")},
		}}},
	})
	must(t, err)
	if result.Code != api.CodeDurableAck {
		t.Fatalf("cannot seed non-JSON native value: %+v", result)
	}
	err = f.store.View(context.Background(), func(tx *Tx) error {
		var value any
		found, err := tx.Get("broken", &value)
		if !found {
			t.Fatal("corrupt record reported absent")
		}
		return err
	})
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt JSON not reported: %v", err)
	}
}

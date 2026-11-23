// These tests run the real ATE engine/service in a child process over stdio.
package atestore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	ate "github.com/vkviyu/agent-temporal-engine"
	"github.com/vkviyu/agent-temporal-engine/agentprofile"
	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/memory"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/agent-temporal-engine/service"
	"github.com/vkviyu/agent-temporal-engine/spacecatalog"
	"github.com/vkviyu/agent-temporal-engine/transport"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateprocess"
	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

const helperEnvironment = "GSEA_ATESTORE_NATIVE_TEST"
const dropEnvironment = "GSEA_ATESTORE_DROP_APPEND"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnvironment) == "1" && len(os.Args) > 2 {
		if os.Args[1] == "init-state" || os.Args[1] == "serve" {
			if err := runNativeHelper(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

type nativeBackend struct {
	*ate.Store
	stream transport.Stream
}

// Fail loudly if a regression introduces full-state host reads.
func (b *nativeBackend) GetAgentState(context.Context, api.ReadAgentStateRequest) (api.AgentStateResult, error) {
	return api.AgentStateResult{}, errors.New("full-state reads forbidden in atestore")
}

func (b *nativeBackend) Commit(ctx context.Context, r api.CommitRequest) (api.CommitResult, error) {
	mode := os.Getenv(dropEnvironment)
	command, _ := agentprofile.DecodeCommand(r.Body)
	drop := r.Method == api.MethodApplyAgent && command.AppendSessionEvents != nil
	if strings.HasPrefix(mode, "upload-") {
		drop = r.Method == api.MethodMutateState && strings.HasPrefix(string(r.RequestID), requestPrefix+"upload-")
		mode = strings.TrimPrefix(mode, "upload-")
	}
	if drop && mode == "before" {
		_ = b.stream.Close()
		return api.CommitResult{}, errors.New("injected disconnect before commit")
	}
	if drop && mode == "rejected" {
		return api.CommitResult{Code: api.CodeRejected, RequestID: r.RequestID}, nil
	}
	result, err := b.Store.Commit(ctx, r)
	if drop && mode == "after" {
		_ = b.stream.Close()
	}
	return result, err
}

func runNativeHelper() error {
	directory := os.Args[len(os.Args)-1]
	options := ate.Options{AgentProfile: true, EventProfile: true}
	if os.Args[1] == "init-state" {
		store, err := ate.Create(directory, options)
		if err != nil {
			return err
		}
		defer store.Close()
		_, err = store.ApplySpace(context.Background(), api.ApplySpaceRequest{
			Key:     model.RequestKey{Space: "session-history", Principal: "gsea", RequestID: "create-history-space"},
			Command: spacecatalog.Apply{Name: "Session history"},
		})
		return err
	}
	store, report, err := ate.OpenExisting(directory, options)
	if err != nil {
		return err
	}
	defer store.Close()
	if !report.Verified {
		return errors.New("native recovery not verified")
	}
	stream, err := transport.NewPipes(os.Stdin, os.Stdout)
	if err != nil {
		return err
	}
	principal := model.PrincipalID("gsea")
	if os.Getenv("GSEA_ATESTORE_FOREIGN_PRINCIPAL") == "1" {
		principal = "foreign"
	}
	server, err := service.New(&nativeBackend{Store: store, stream: stream}, service.Options{
		Credentials: map[string]service.Credential{
			os.Getenv("GSEA_ATE_PRIVATE_LAUNCH_TOKEN"): {Principal: principal, AllSpaces: true, AllowGlobalSnapshot: true},
		},
	})
	if err != nil {
		return err
	}
	// Injected transport loss is expected; the engine still drains and closes.
	_ = server.ServeStream(context.Background(), stream)
	return server.Shutdown(context.Background())
}

// The test PendingStore stores ONLY unresolved serialized command envelopes.
// Reopening replaces this object; native committed history needs none of it.
type pendingStore struct {
	mu        sync.Mutex
	rows      map[api.RequestKey][]byte
	attempted map[api.RequestKey]bool
	rejected  map[api.RequestKey]api.CommitResult
}

func newPending() *pendingStore {
	return &pendingStore{rows: make(map[api.RequestKey][]byte),
		attempted: make(map[api.RequestKey]bool), rejected: make(map[api.RequestKey]api.CommitResult)}
}

func (p *pendingStore) Insert(_ context.Context, record aterequest.Record) error {
	raw, err := aterequest.EncodeRecord(record)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	key := record.Request.Key()
	if _, rejected := p.rejected[key]; rejected {
		return aterequest.ErrRejected
	}
	if old, found := p.rows[key]; found && !bytes.Equal(old, raw) {
		return aterequest.ErrConflict
	}
	p.rows[key] = raw
	p.attempted[key] = p.attempted[key] || record.Attempted
	return nil
}

func (p *pendingStore) Get(_ context.Context, key api.RequestKey) (aterequest.Record, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	raw, found := p.rows[key]
	if !found {
		return aterequest.Record{}, false, nil
	}
	record, err := aterequest.DecodeRecord(raw)
	record.Attempted = p.attempted[key]
	return record, true, err
}

func (p *pendingStore) List(_ context.Context, options aterequest.ListOptions) ([]aterequest.Record, error) {
	if err := options.Validate(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var records []aterequest.Record
	total := 0
	for _, raw := range p.rows {
		record, err := aterequest.DecodeRecord(raw)
		if err != nil {
			return nil, err
		}
		if options.After != nil && compareKeys(record.Request.Key(), *options.After) <= 0 {
			continue
		}
		record.Attempted = p.attempted[record.Request.Key()]
		total += len(raw)
		if total > options.MaxBytes {
			return nil, aterequest.ErrLimit
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return compareKeys(records[i].Request.Key(), records[j].Request.Key()) < 0 })
	if len(records) > options.Limit {
		records = records[:options.Limit]
	}
	return records, nil
}

func compareKeys(a, b api.RequestKey) int {
	for _, pair := range [][2]string{{string(a.Space), string(b.Space)}, {string(a.Principal), string(b.Principal)}, {string(a.RequestID), string(b.RequestID)}} {
		if n := strings.Compare(pair[0], pair[1]); n != 0 {
			return n
		}
	}
	return 0
}

func (p *pendingStore) DeleteAcknowledged(_ context.Context, record aterequest.Record, result api.CommitResult) error {
	if err := aterequest.VerifyAcknowledgement(record, result); err != nil {
		return err
	}
	raw, err := aterequest.EncodeRecord(record)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	key := record.Request.Key()
	if previous, found := p.rows[key]; found && !bytes.Equal(previous, raw) {
		return aterequest.ErrConflict
	}
	delete(p.rows, key)
	delete(p.attempted, key)
	return nil
}

func (p *pendingStore) BeginAttempt(_ context.Context, record aterequest.Record) (bool, error) {
	raw, err := aterequest.EncodeRecord(record)
	if err != nil {
		return false, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	key := record.Request.Key()
	if !bytes.Equal(p.rows[key], raw) {
		return false, aterequest.ErrConflict
	}
	if p.attempted[key] {
		return false, nil
	}
	p.attempted[key] = true
	return true, nil
}

func (p *pendingStore) DeleteRejected(_ context.Context, record aterequest.Record, result api.CommitResult) error {
	if err := aterequest.VerifyRejection(record, result); err != nil {
		return err
	}
	raw, err := aterequest.EncodeRecord(record)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	key := record.Request.Key()
	if !bytes.Equal(p.rows[key], raw) {
		return aterequest.ErrConflict
	}
	p.rejected[key] = result
	delete(p.rows, key)
	delete(p.attempted, key)
	return nil
}

type nativeFixture struct {
	store     *Store
	process   *ateprocess.Manager
	pending   *pendingStore
	directory string
	storeID   model.StoreID
}

func newNativeFixture(t *testing.T) *nativeFixture {
	t.Helper()
	t.Setenv(helperEnvironment, "1")
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	t.Setenv(dropEnvironment, "")
	f := &nativeFixture{directory: filepath.Join(t.TempDir(), "native-ate"), pending: newPending()}
	f.reopen(t, f.pending)
	return f
}

func (f *nativeFixture) reopen(t *testing.T, pending *pendingStore) {
	t.Helper()
	if f.process != nil {
		if err := f.process.Close(); err != nil {
			t.Fatal(err)
		}
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	process, err := ateprocess.New(ateprocess.Options{Binary: binary, Directory: f.directory, Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Close() })
	if err := process.WithClient(t.Context(), func(c *client.Client) error {
		snapshot, err := c.Snapshot(t.Context())
		if err != nil {
			return err
		}
		if f.storeID != "" && f.storeID != snapshot.Fence.Store {
			return ErrIdentity
		}
		f.storeID = snapshot.Fence.Store
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.process, f.pending = process, pending
	f.store, err = New(Options{Process: process, Pending: pending, Space: "session-history", StoreID: f.storeID})
	if err != nil {
		t.Fatal(err)
	}
}

func pendingEvent(correlation string) session.PendingEvent {
	return session.PendingEvent{Type: session.EventUserMessage,
		Payload:     json.RawMessage(`{"message": "<original>", "number": 1.00}`),
		CausationID: "original-cause", CorrelationID: correlation, RuntimeSnapshotID: "snapshot-original"}
}

func nativeFailure(t *testing.T, err error) {
	t.Helper()
	// ateprocess intentionally redacts messages but retains error causes.
	for current := err; current != nil; current = errors.Unwrap(current) {
		t.Logf("%T: %v", current, current)
	}
	t.Fatal(err)
}

func TestNativeAppendLoadReopenAndEvidence(t *testing.T) {
	f := newNativeFixture(t)
	ctx := t.Context()
	original, err := f.store.Append(ctx, "original-session", 0, pendingEvent("call"), pendingEvent("other"), pendingEvent("call"))
	if err != nil {
		nativeFailure(t, err)
	}
	if len(f.pending.rows) != 0 {
		t.Fatal("durable history retained as pending")
	}
	if !bytes.Equal(original[0].Payload, pendingEvent("call").Payload) {
		t.Fatal("original payload changed")
	}
	loaded, err := f.store.Load(ctx, "original-session", 0, 10)
	if err != nil || !reflect.DeepEqual(original, loaded) {
		t.Fatalf("load changed envelope: %v", err)
	}
	correlated, err := f.store.LoadByCorrelation(ctx, "original-session", "call")
	if err != nil || !reflect.DeepEqual(correlated, []session.Event{original[0], original[2]}) {
		t.Fatalf("correlation read: %v", err)
	}
	if _, err := f.store.Append(ctx, "original-session", 0, pendingEvent("stale")); !errors.Is(err, session.ErrSequenceConflict) {
		t.Fatalf("stale sequence: %v", err)
	}
	if sequence, err := f.store.CurrentSequence(ctx, "original-session"); err != nil || sequence != 3 {
		t.Fatalf("sequence: %d %v", sequence, err)
	}
	// Throw away all local state and all clients. Each new child must open
	// the actual durable ATE store; there is no SQLite event history.
	f.reopen(t, newPending())
	reopened, err := f.store.Load(ctx, "original-session", 0, 10)
	if err != nil || !reflect.DeepEqual(original, reopened) {
		t.Fatalf("reopen changed identity: %v", err)
	}
	records, err := f.store.History(ctx, "original-session", 0, 10)
	if err != nil || len(records) != 3 {
		t.Fatalf("history: %v", err)
	}
	for i, record := range records {
		var event session.Event
		if err := json.Unmarshal(record.Payload, &event); err != nil || !reflect.DeepEqual(event, original[i]) {
			t.Fatalf("history envelope: %v", err)
		}
	}
	var before model.CommitFence
	if err := f.process.WithClient(ctx, func(c *client.Client) error {
		snapshot, err := c.Snapshot(ctx)
		before = snapshot.Fence
		return err
	}); err != nil {
		t.Fatal(err)
	}
	sources, err := f.store.EvidenceSources(ctx, records[0])
	if err != nil {
		nativeFailure(t, err)
	}
	if len(sources) != 1 {
		t.Fatalf("existing source resolution returned %d sources", len(sources))
	}
	if err := f.process.WithClient(ctx, func(c *client.Client) error {
		snapshot, err := c.Snapshot(ctx)
		if err != nil {
			return err
		}
		if snapshot.Fence != before {
			return errors.New("source resolution wrote an evidence copy")
		}
		page, err := c.ListSessionEvents(ctx, api.ListSessionEventsRequest{Space: "session-history", Principal: "gsea",
			SessionID: "original-session", ThroughSequence: 1, Limit: 1})
		if err != nil {
			return err
		}
		if page.Items[0].Payload != sources[0].SourceID {
			return errors.New("source is not existing SessionEvent payload")
		}
		candidate := memory.Memory{Space: "session-history", Kind: memory.KindFact,
			Claim: []byte("a fact supported by original session evidence"), Sources: sources}
		result, err := c.ApplyMemory(ctx, api.ApplyMemoryRequest{
			Key:     model.RequestKey{Space: "session-history", Principal: "gsea", RequestID: "native-evidence-candidate"},
			Command: memory.Command{PutCandidate: &candidate},
		})
		if err != nil {
			return err
		}
		if result.Code != api.CodeDurableAck {
			return fmt.Errorf("memory rejected existing event source: %s", result.Code)
		}
		return nil
	}); err != nil {
		nativeFailure(t, err)
	}
	forged := records[0]
	forged.SourceID = "not-original"
	if sources, err := f.store.EvidenceSources(ctx, forged); err == nil || sources != nil {
		t.Fatal("forged source resolved")
	}
}

func TestNativeUnknownAppendReconciliation(t *testing.T) {
	for _, mode := range []string{"before", "after"} {
		t.Run(mode, func(t *testing.T) {
			f := newNativeFixture(t)
			t.Setenv(dropEnvironment, mode)
			input := []session.PendingEvent{pendingEvent("uncertain"), pendingEvent("uncertain")}
			if _, err := f.store.Append(t.Context(), "uncertain-session", session.AnySequence, input...); !errors.Is(err, aterequest.ErrUnknown) {
				t.Fatalf("lost commit response was not unknown: %v", err)
			}
			records, err := f.pending.List(t.Context(), aterequest.ListOptions{Limit: 128, MaxBytes: aterequest.MaxListBytes})
			if err != nil || len(records) != 1 {
				t.Fatalf("original command not retained: %d %v", len(records), err)
			}
			command, err := agentprofile.DecodeCommand(records[0].Request.Body)
			if err != nil {
				t.Fatal(err)
			}
			original, err := f.store.commandEvents(command.AppendSessionEvents)
			if err != nil {
				t.Fatal(err)
			}
			// Rehydrate ONLY the transient command envelope after process/store
			// reopen. It is removed after receipt verification.
			recoveredPending := newPending()
			if err := recoveredPending.Insert(t.Context(), records[0]); err != nil {
				t.Fatal(err)
			}
			t.Setenv(dropEnvironment, "")
			f.reopen(t, recoveredPending)
			if _, err := f.store.Append(t.Context(), "uncertain-session", session.AnySequence, pendingEvent("changed")); !errors.Is(err, ErrPending) {
				t.Fatalf("pending operation identity replaced: %v", err)
			}
			recovered, err := f.store.Append(t.Context(), "uncertain-session", session.AnySequence, input...)
			if mode == "before" {
				// Native receipts prove acknowledgements, not rejections. Once
				// attempted, absence cannot authorize resubmission.
				if !errors.Is(err, aterequest.ErrUnknown) || recovered != nil {
					t.Fatalf("ambiguous absence retried: %v", err)
				}
				retained, found, err := f.pending.Get(t.Context(), records[0].Request.Key())
				if err != nil || !found || !retained.Attempted || !aterequest.Equal(retained, records[0]) {
					t.Fatalf("unknown attempt lost original envelope: %v", err)
				}
				if sequence, err := f.store.CurrentSequence(t.Context(), "uncertain-session"); err != nil || sequence != 0 {
					t.Fatalf("ambiguous attempt changed native history: %d %v", sequence, err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(original, recovered) {
				t.Fatalf("reconciliation regenerated identities or times: %v", err)
			}
			if len(f.pending.rows) != 0 {
				t.Fatal("acknowledged command not cleaned up")
			}
			loaded, err := f.store.Load(t.Context(), "uncertain-session", 0, 10)
			if err != nil || !reflect.DeepEqual(original, loaded) {
				t.Fatalf("duplicated/lost uncertain append: %v", err)
			}
		})
	}
}

func TestNativeFrozenHistoryAndPagination(t *testing.T) {
	f := newNativeFixture(t)
	ctx := t.Context()
	empty, err := f.store.HistoryCut(ctx, "paged")
	if err != nil || empty != 0 {
		t.Fatalf("empty cut: %v", err)
	}
	for batch := 0; batch < 5; batch++ {
		pending := make([]session.PendingEvent, agentprofile.MaxSessionAppendEvents)
		for i := range pending {
			pending[i] = pendingEvent("page-call")
		}
		if _, err := f.store.Append(ctx, "paged", uint64(batch*len(pending)), pending...); err != nil {
			nativeFailure(t, err)
		}
	}
	cut, err := f.store.HistoryCut(ctx, "paged")
	if err != nil || cut != 320 {
		t.Fatalf("cut: %d %v", cut, err)
	}
	if _, err := f.store.Append(ctx, "paged", session.AnySequence, pendingEvent("later")); err != nil {
		t.Fatal(err)
	}
	history, err := f.store.HistoryThrough(ctx, "paged", 0, cut, 1000)
	if err != nil || len(history) != 320 || history[319].Sequence != cut {
		t.Fatalf("frozen typed pagination: %d %v", len(history), err)
	}
	first, err := f.store.HistoryThrough(ctx, "paged", 0, cut, 17)
	if err != nil || len(first) != 17 {
		t.Fatalf("short page: %v", err)
	}
	next, err := f.store.HistoryThrough(ctx, "paged", 17, cut, 1000)
	if err != nil || len(next) != 303 || next[0].Sequence != 18 {
		t.Fatalf("next cut page: %v", err)
	}
	emptyPage, err := f.store.HistoryThrough(ctx, "paged", 0, empty, 1000)
	if err != nil || len(emptyPage) != 0 {
		t.Fatalf("zero cut drifted: %v", err)
	}
	if _, err := f.store.HistoryThrough(ctx, "paged", 0, 999, 10); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("unavailable cut silently clamped: %v", err)
	}
	correlated, err := f.store.LoadByCorrelation(ctx, "paged", "page-call")
	if err != nil || len(correlated) != 320 {
		t.Fatalf("correlation pagination: %d %v", len(correlated), err)
	}
}

func TestNativeBoundsAreAtomic(t *testing.T) {
	f := newNativeFixture(t)
	ctx := t.Context()
	cases := []struct {
		name    string
		pending []session.PendingEvent
		want    error
	}{
		{"count", make([]session.PendingEvent, 65), ErrLimit},
		{"event-bytes", []session.PendingEvent{{Type: session.EventUserMessage, Payload: json.RawMessage(`"` + strings.Repeat("x", MaxAppendBytes) + `"`)}}, ErrEventTooLarge},
		{"envelope-overhead", []session.PendingEvent{{Type: session.EventUserMessage, Payload: json.RawMessage(`"` + strings.Repeat("x", MaxAppendBytes-100) + `"`)}}, ErrEventTooLarge},
	}
	largeBatch := make([]session.PendingEvent, 6)
	for i := range largeBatch {
		largeBatch[i] = session.PendingEvent{Type: session.EventUserMessage, Payload: json.RawMessage(`"` + strings.Repeat("x", 600<<10) + `"`)}
	}
	cases = append(cases, struct {
		name    string
		pending []session.PendingEvent
		want    error
	}{"command-bytes", largeBatch, ErrLimit})
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := f.store.Append(ctx, session.ID(test.name), 0, test.pending...); !errors.Is(err, test.want) {
				t.Fatalf("bound: got %v, want %v", err, test.want)
			}
			if len(f.pending.rows) != 0 {
				t.Fatal("oversize input reached pending storage")
			}
			if err := f.process.WithClient(ctx, func(c *client.Client) error {
				found, err := c.GetSession(ctx, api.ReadSessionRequest{Space: "session-history", Principal: "gsea", SessionID: test.name})
				if found.Found {
					return errors.New("invalid append created or partially wrote session")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNativeAuthorityValidation(t *testing.T) {
	f := newNativeFixture(t)
	wrong, err := New(Options{Process: f.process, Pending: f.pending, Space: "session-history", StoreID: "wrong-store"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrong.CurrentSequence(t.Context(), "session"); !errors.Is(err, ErrIdentity) {
		t.Fatalf("store identity: %v", err)
	}
	t.Setenv("GSEA_ATESTORE_FOREIGN_PRINCIPAL", "1")
	if _, err := f.store.CurrentSequence(t.Context(), "session"); !errors.Is(err, ErrIdentity) {
		t.Fatalf("principal: %v", err)
	}
}

type executorFunc func(context.Context, api.CommitRequest) (api.CommitResult, error)

func (fn executorFunc) Execute(ctx context.Context, request api.CommitRequest) (api.CommitResult, error) {
	return fn(ctx, request)
}

func TestNativeSequenceCASLosesToConcurrentWriter(t *testing.T) {
	f := newNativeFixture(t)
	ctx := t.Context()
	if _, err := f.store.Append(ctx, "race", 0, pendingEvent("first")); err != nil {
		t.Fatal(err)
	}
	injected := false
	f.store.newExecutor = func(c *client.Client, id model.StoreID) (Executor, error) {
		executor, err := aterequest.NewExecutor(c, f.pending, id)
		if err != nil {
			return nil, err
		}
		return executorFunc(func(ctx context.Context, request api.CommitRequest) (api.CommitResult, error) {
			if !injected {
				injected = true
				winner, err := makeEvents("race", 1, []session.PendingEvent{pendingEvent("winner")})
				if err != nil {
					return api.CommitResult{}, err
				}
				competing, err := f.store.appendRequest("race", 1, winner)
				if err != nil {
					return api.CommitResult{}, err
				}
				result, err := c.Commit(ctx, competing)
				if err != nil || result.Code != api.CodeDurableAck {
					return result, fmt.Errorf("competing writer: %v", err)
				}
			}
			return executor.Execute(ctx, request)
		}), nil
	}
	if _, err := f.store.Append(ctx, "race", 1, pendingEvent("loser")); !errors.Is(err, session.ErrSequenceConflict) {
		nativeFailure(t, fmt.Errorf("native CAS did not report sequence conflict: %w", err))
	}
	events, err := f.store.Load(ctx, "race", 0, 10)
	if err != nil || len(events) != 2 || events[1].CorrelationID != "winner" {
		t.Fatalf("CAS failure changed history: %v", err)
	}
	if len(f.pending.rows) != 0 || len(f.pending.rejected) != 1 {
		t.Fatal("conclusive rejection did not retire its request payload")
	}
	if _, err := f.store.Append(ctx, "race", 2, pendingEvent("next")); err != nil {
		t.Fatalf("retired rejection blocked a new CAS: %v", err)
	}
}

func TestNativeBytePagingAndExactPayload(t *testing.T) {
	f := newNativeFixture(t)
	events := make([]session.PendingEvent, 30)
	for i := range events {
		events[i] = pendingEvent("")
		events[i].Payload = json.RawMessage(" \n{\"body\": \"" + strings.Repeat("x", 30<<10) + "\", \"n\":1.00}\t\n")
	}
	original, err := f.store.Append(t.Context(), "byte-pages", 0, events...)
	if err != nil {
		nativeFailure(t, err)
	}
	loaded, err := f.store.Load(t.Context(), "byte-pages", 0, 1000)
	if err != nil || !reflect.DeepEqual(loaded, original) {
		t.Fatalf("byte-paged envelopes changed: %v", err)
	}
	for i := range original {
		if !bytes.Equal(events[i].Payload, original[i].Payload) {
			t.Fatal("payload framing or number spelling changed")
		}
	}
	emptyCorrelation, err := f.store.LoadByCorrelation(t.Context(), "byte-pages", "")
	if err != nil || !reflect.DeepEqual(emptyCorrelation, original) {
		t.Fatalf("empty correlation changed semantics: %v", err)
	}
	history, err := f.store.History(t.Context(), "byte-pages", 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.EvidenceSources(t.Context(), history[0]); err != nil {
		nativeFailure(t, err)
	}
}

func TestNativeCorruptEnvelopeFailsWholePage(t *testing.T) {
	f := newNativeFixture(t)
	ctx := t.Context()
	if _, err := f.store.Append(ctx, "corrupt", 0, pendingEvent("valid")); err != nil {
		t.Fatal(err)
	}
	events, err := makeEvents("corrupt", 1, []session.PendingEvent{pendingEvent("corrupt")})
	if err != nil {
		t.Fatal(err)
	}
	host, err := encodeEvent(events[0])
	if err != nil {
		t.Fatal(err)
	}
	host.Payload = bytes.Replace(host.Payload, []byte("<original>"), []byte("<modified>"), 1)
	if err := f.process.WithClient(ctx, func(c *client.Client) error {
		ack, err := c.ApplyAgent(ctx, api.ApplyAgentRequest{
			Key: model.RequestKey{Space: "session-history", Principal: "gsea", RequestID: "corrupt-host-checksum"},
			Command: agentprofile.Command{AppendSessionEvents: &agentprofile.AppendSessionEventsCommand{
				Session: "corrupt", ExpectedSequence: 1, Events: []agentprofile.HostEvent{host},
			}},
		})
		if err != nil {
			return err
		}
		if ack.Code != api.CodeDurableAck {
			return fmt.Errorf("opaque native append: %s", ack.Code)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if events, err := f.store.Load(ctx, "corrupt", 0, 10); !errors.Is(err, ErrCorrupt) || events != nil {
		t.Fatalf("corrupt page exposed partial events: %v", err)
	}
	if records, err := f.store.HistoryThrough(ctx, "corrupt", 0, 2, 10); !errors.Is(err, ErrCorrupt) || records != nil {
		t.Fatalf("corrupt history exposed a partial cut: %v", err)
	}
}

type shortPending struct{ *pendingStore }

func (p shortPending) List(ctx context.Context, options aterequest.ListOptions) ([]aterequest.Record, error) {
	options.Limit = 1
	return p.pendingStore.List(ctx, options)
}

func TestPendingShortPageDoesNotProveAbsence(t *testing.T) {
	f := newNativeFixture(t)
	for _, id := range []string{"first", "second"} {
		record, err := aterequest.NewRecord(f.storeID, api.CommitRequest{
			Space: "session-history", Principal: "gsea", RequestID: model.RequestID(id), Method: "other/v1", Body: []byte("original"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.pending.Insert(t.Context(), record); err != nil {
			t.Fatal(err)
		}
	}
	f.store.pending = shortPending{f.pending}
	if _, err := f.store.Append(t.Context(), "not-created", 0, pendingEvent("x")); !errors.Is(err, ErrLimit) {
		t.Fatalf("short pending page accepted as exhaustive: %v", err)
	}
	if sequence, err := f.store.CurrentSequence(t.Context(), "not-created"); err != nil || sequence != 0 {
		t.Fatalf("incomplete recovery changed history: %d %v", sequence, err)
	}
}

type cleanupPending struct {
	*pendingStore
	fail bool
}

func (p *cleanupPending) DeleteAcknowledged(ctx context.Context, record aterequest.Record, ack api.CommitResult) error {
	command, _ := agentprofile.DecodeCommand(record.Request.Body)
	if command.AppendSessionEvents != nil && p.fail {
		p.fail = false
		return errors.New("injected pending cleanup failure")
	}
	return p.pendingStore.DeleteAcknowledged(ctx, record, ack)
}

func TestAcknowledgedCleanupFailureRecoversOriginal(t *testing.T) {
	f := newNativeFixture(t)
	pending := &cleanupPending{pendingStore: f.pending, fail: true}
	store, err := New(Options{Process: f.process, Pending: pending, Space: "session-history", StoreID: f.storeID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(t.Context(), "cleanup", 0, pendingEvent("same")); !errors.Is(err, aterequest.ErrCleanup) {
		t.Fatalf("known commit cleanup error: %v", err)
	}
	durable, err := store.Load(t.Context(), "cleanup", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := store.Append(t.Context(), "cleanup", 0, pendingEvent("same"))
	if err != nil || !reflect.DeepEqual(durable, recovered) {
		t.Fatalf("cleanup retry changed committed events: %v", err)
	}
}

func TestSavedUnattemptedAppendUsesOriginalEnvelope(t *testing.T) {
	f := newNativeFixture(t)
	ctx := t.Context()
	if _, err := f.store.Append(ctx, "prepared", 0, pendingEvent("first")); err != nil {
		t.Fatal(err)
	}
	input := []session.PendingEvent{pendingEvent("saved")}
	original, err := makeEvents("prepared", 1, input)
	if err != nil {
		t.Fatal(err)
	}
	request, err := f.store.appendRequest("prepared", 1, original)
	if err != nil {
		t.Fatal(err)
	}
	record, err := aterequest.NewRecord(f.storeID, request)
	if err != nil {
		t.Fatal(err)
	}
	recoveredPending := newPending()
	if err := recoveredPending.Insert(ctx, record); err != nil {
		t.Fatal(err)
	}
	f.reopen(t, recoveredPending)
	recovered, err := f.store.Append(ctx, "prepared", 1, input...)
	if err != nil || !reflect.DeepEqual(recovered, original) {
		t.Fatalf("unattempted request regenerated original identity: %v", err)
	}
}

type lateErrorProcess struct{ Process }

func (p lateErrorProcess) WithClient(ctx context.Context, fn func(*client.Client) error) error {
	if err := p.Process.WithClient(ctx, fn); err != nil {
		return err
	}
	return errors.New("injected failure after callback and durable acknowledgement")
}

func TestVerifiedAckSurvivesLateProcessFailure(t *testing.T) {
	f := newNativeFixture(t)
	store, err := New(Options{Process: lateErrorProcess{f.process}, Pending: f.pending, Space: "session-history", StoreID: f.storeID})
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.Append(t.Context(), "late-error", session.AnySequence, pendingEvent("one"))
	if err != nil || len(events) != 1 {
		t.Fatalf("verified commit became a failed append: %v", err)
	}
	if sequence, err := f.store.CurrentSequence(t.Context(), "late-error"); err != nil || sequence != 1 {
		t.Fatalf("late process failure changed commit: %d %v", sequence, err)
	}
}

func TestPayloadReferenceRequiresVerifiedReadback(t *testing.T) {
	native := agentprofile.SessionEvent{Event: agentprofile.HostEvent{
		PayloadRef: agentprofile.HostPayloadRef{Bytes: 1},
	}}
	if _, err := decodeEvent(native); !errors.Is(err, ErrPayloadReference) {
		t.Fatalf("referenced payload mistaken for an inline envelope: %v", err)
	}
}

func TestNativeLongOriginalIdentifiers(t *testing.T) {
	f := newNativeFixture(t)
	id := session.ID(strings.Repeat("s", model.MaxIdentityBytes))
	correlation := strings.Repeat("c", model.MaxIdentityBytes)
	original, err := f.store.Append(t.Context(), id, 0, pendingEvent(correlation))
	if err != nil {
		nativeFailure(t, err)
	}
	loaded, err := f.store.LoadByCorrelation(t.Context(), id, correlation)
	if err != nil || !reflect.DeepEqual(original, loaded) || loaded[0].SessionID != id {
		t.Fatalf("original identifier changed or became unreadable: %v", err)
	}
	if _, err := f.store.HistoryThrough(t.Context(), string(id), 999, 999, 1); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("empty out-of-range cut accepted: %v", err)
	}
	for _, input := range []struct {
		id          session.ID
		correlation string
	}{
		{id + "s", correlation}, {id, correlation + "c"},
	} {
		if _, err := f.store.Append(t.Context(), input.id, 1, pendingEvent(input.correlation)); !errors.Is(err, session.ErrInvalidEvent) {
			t.Fatalf("identifier exceeding native publication bound reached write: %v", err)
		}
	}
}

type processFunc func(context.Context, func(*client.Client) error) error

func (fn processFunc) WithClient(ctx context.Context, callback func(*client.Client) error) error {
	return fn(ctx, callback)
}

// The real request executor persists BeginAttempt before this client loses the
// response. Waiting for cancellation prevents its in-lease lookup from succeeding.
type timeoutCommitClient struct {
	*client.Client
	pending  aterequest.PendingStore
	target   string
	commit   bool
	captured chan []aterequest.Record
}

func (c timeoutCommitClient) Commit(ctx context.Context, request api.CommitRequest) (api.CommitResult, error) {
	if request.Method != c.target {
		return c.Client.Commit(ctx, request)
	}
	records, err := aterequest.ListPending(ctx, c.pending)
	if err != nil {
		return api.CommitResult{}, err
	}
	c.captured <- records
	if c.commit {
		result, err := c.Client.Commit(ctx, request)
		if err != nil || result.Code != api.CodeDurableAck {
			return result, fmt.Errorf("injected commit failed: %w", err)
		}
	}
	<-ctx.Done()
	return api.CommitResult{}, ctx.Err()
}

type attemptPending struct {
	*pendingStore
	mu       sync.Mutex
	attempts []api.RequestKey
}

func (p *attemptPending) BeginAttempt(ctx context.Context, record aterequest.Record) (bool, error) {
	p.mu.Lock()
	p.attempts = append(p.attempts, record.Request.Key())
	p.mu.Unlock()
	return p.pendingStore.BeginAttempt(ctx, record)
}

func TestNativeAppendFreshReconciliation(t *testing.T) {
	for _, test := range []struct {
		name         string
		large        bool
		upload       bool
		commit       bool
		deadlineOnly bool
		disabled     bool
	}{
		{name: "expired-append-ack", commit: true},
		{name: "expired-append-absent"},
		{name: "deadline-without-unknown", commit: true, deadlineOnly: true},
		{name: "zero-disables", commit: true, disabled: true},
		{name: "expired-upload-ack", large: true, upload: true, commit: true},
		{name: "expired-upload-absent", large: true, upload: true},
		{name: "large-expired-append-ack", large: true, commit: true},
		{name: "large-expired-append-absent", large: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newNativeFixture(t)
			if _, err := f.store.Append(t.Context(), "fresh", 0, pendingEvent("seed")); err != nil {
				t.Fatal(err)
			}
			pending := &attemptPending{pendingStore: f.pending}
			type contextKey struct{}
			// Expire the actual caller/first-batch context after the durable
			// commit (or withheld submission), not just a fabricated error.
			ctx, cancel := context.WithTimeout(context.WithValue(t.Context(), contextKey{}, "preserved"), time.Second)
			defer cancel()
			captured := make(chan []aterequest.Record, 1)
			callbackDone := make(chan struct{})
			batches := 0
			process := processFunc(func(batch context.Context, fn func(*client.Client) error) error {
				batches++
				if batches == 2 && !test.disabled {
					select {
					case <-callbackDone:
					default:
						t.Error("recovery started before original callback joined")
					}
					if batch.Err() != nil || batch.Value(contextKey{}) != "preserved" {
						t.Error("recovery inherited cancellation or lost context values")
					}
					deadline, ok := batch.Deadline()
					if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second {
						t.Error("recovery did not have a fresh bounded deadline")
					}
				}
				return f.process.WithClient(batch, fn)
			})
			timeout := 5 * time.Second
			if test.disabled {
				timeout = 0
			}
			store, err := New(Options{Process: process, Pending: pending, Space: f.store.space,
				StoreID: f.storeID, ReconciliationTimeout: timeout,
				NewExecutor: func(c *client.Client, id model.StoreID) (Executor, error) {
					target := api.MethodApplyAgent
					if test.upload {
						target = api.MethodMutateState
					}
					executor, err := aterequest.NewExecutor(timeoutCommitClient{Client: c, pending: pending,
						target: target, commit: test.commit, captured: captured}, pending, id)
					if err != nil {
						return nil, err
					}
					return executorFunc(func(ctx context.Context, request api.CommitRequest) (api.CommitResult, error) {
						result, err := executor.Execute(ctx, request)
						if request.Method == target {
							defer close(callbackDone)
							if test.deadlineOnly {
								return api.CommitResult{}, context.DeadlineExceeded
							}
						}
						return result, err
					}), nil
				}})
			if err != nil {
				t.Fatal(err)
			}
			input := []session.PendingEvent{pendingEvent("original"), pendingEvent("tail")}
			if test.large {
				input[0] = largePending(400 << 10)
			}
			got, appendErr := store.Append(ctx, "fresh", session.AnySequence, input...)
			var records []aterequest.Record
			select {
			case records = <-captured:
			default:
				t.Fatalf("first batch failed before injection: %v", appendErr)
			}
			wantBatches := 2
			if test.disabled {
				wantBatches = 1
			}
			if batches != wantBatches || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				t.Fatalf("batches=%d, original context=%v", batches, ctx.Err())
			}
			var plan appendPlan
			for _, record := range records {
				if record.Request.Method == api.MethodApplyAgent {
					plan.Request = record.Request
				} else {
					request := record.Request
					plan.Upload = &request
				}
			}
			command, err := agentprofile.DecodeCommand(plan.Request.Body)
			if err != nil || command.AppendSessionEvents.ExpectedSequence != 1 {
				t.Fatalf("AnySequence did not retain original CAS: %v", err)
			}
			success := test.commit && !test.disabled
			if success {
				if appendErr != nil {
					t.Fatalf("fresh recovery failed: %v", appendErr)
				}
				var original []session.Event
				if err := f.process.WithClient(t.Context(), func(c *client.Client) error {
					var err error
					original, err = store.recoverEvents(t.Context(), c, plan)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(original, got) || len(f.pending.rows) != 0 {
					t.Fatal("recovery changed original event IDs/times/bytes or retained pending")
				}
			} else {
				if got != nil || !errors.Is(appendErr, aterequest.ErrUnknown) {
					t.Fatalf("ambiguous/disabled append resolved unexpectedly: %v", appendErr)
				}
				for _, original := range records {
					retained, found, err := pending.Get(t.Context(), original.Request.Key())
					if err != nil || !found || !aterequest.Equal(retained, original) || retained.Attempted != original.Attempted {
						t.Fatalf("original pending record/attempt changed: %v", err)
					}
				}
				if _, err := store.Append(t.Context(), "fresh", session.AnySequence, pendingEvent("different")); !errors.Is(err, ErrPending) {
					t.Fatalf("different append was not blocked: %v", err)
				}
			}
			loaded, err := f.store.Load(t.Context(), "fresh", 1, 10)
			wantCount := 0
			if success || test.disabled {
				wantCount = 2
			}
			if err != nil || len(loaded) != wantCount || (success && !reflect.DeepEqual(got, loaded)) {
				t.Fatalf("native history duplicated or changed: %d %v", len(loaded), err)
			}
			pending.mu.Lock()
			attempts := append([]api.RequestKey(nil), pending.attempts...)
			pending.mu.Unlock()
			counts := make(map[api.RequestKey]int)
			for _, key := range attempts {
				counts[key]++
				if counts[key] != 1 {
					t.Fatal("original request acquired another attempt")
				}
			}
			wantAttempts := 1
			if test.large && (!test.upload || success) {
				wantAttempts = 2
			}
			if len(attempts) != wantAttempts || counts[plan.Request.Key()] != boolInt(!test.upload || success) {
				t.Fatalf("unexpected attempts: %v", attempts)
			}
			if test.large && !strings.HasPrefix(string(attempts[0].RequestID), requestPrefix+"upload-") {
				t.Fatal("append attempted before upload acknowledgement")
			}
		})
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestNativeAppendReconciliationInterleaving(t *testing.T) {
	for _, mode := range []string{"cleared-ack", "cleared-absent", "changed-envelope", "mutated-executor", "different-pending", "recovery-deadline"} {
		t.Run(mode, func(t *testing.T) {
			f := newNativeFixture(t)
			if _, err := f.store.Append(t.Context(), "interleaved", 0, pendingEvent("seed")); err != nil {
				t.Fatal(err)
			}
			pending := &attemptPending{pendingStore: f.pending}
			var original aterequest.Record
			var unrelated appendPlan
			batches := 0
			process := processFunc(func(ctx context.Context, fn func(*client.Client) error) error {
				batches++
				if batches == 2 && mode == "recovery-deadline" {
					<-ctx.Done()
					return ctx.Err()
				}
				err := f.process.WithClient(ctx, fn)
				if batches == 1 {
					// Another owner operates after the first lease has ended,
					// before Append can acquire its reconciliation lease.
					switch mode {
					case "cleared-ack", "cleared-absent":
						pending.pendingStore.mu.Lock()
						delete(pending.rows, original.Request.Key())
						delete(pending.attempted, original.Request.Key())
						pending.pendingStore.mu.Unlock()
					case "changed-envelope":
						changed := original.Request
						changed.Body = bytes.Replace(changed.Body, []byte("original-cause"), []byte("changed-cause"), 1)
						record, recordErr := aterequest.NewRecord(f.storeID, changed)
						if recordErr != nil {
							return recordErr
						}
						raw, recordErr := aterequest.EncodeRecord(record)
						if recordErr != nil {
							return recordErr
						}
						pending.pendingStore.mu.Lock()
						pending.rows[changed.Key()] = raw
						pending.pendingStore.mu.Unlock()
					case "different-pending":
						events, prepareErr := makeEvents("interleaved", 2, []session.PendingEvent{pendingEvent("other")})
						if prepareErr != nil {
							return prepareErr
						}
						unrelated, prepareErr = f.store.prepareAppend("interleaved", 2, events)
						if prepareErr != nil {
							return prepareErr
						}
						if saveErr := f.store.savePlan(ctx, unrelated); saveErr != nil {
							return saveErr
						}
					}
				}
				return err
			})
			timeout := 5 * time.Second
			if mode == "recovery-deadline" {
				timeout = 20 * time.Millisecond
			}
			store, err := New(Options{Process: process, Pending: pending, Space: f.store.space, StoreID: f.storeID,
				ReconciliationTimeout: timeout,
				NewExecutor: func(c *client.Client, id model.StoreID) (Executor, error) {
					return executorFunc(func(ctx context.Context, request api.CommitRequest) (api.CommitResult, error) {
						var err error
						original, err = aterequest.NewRecord(id, request)
						if err != nil {
							return api.CommitResult{}, err
						}
						if started, err := pending.BeginAttempt(ctx, original); err != nil || !started {
							return api.CommitResult{}, fmt.Errorf("original attempt: %v", err)
						}
						if mode != "cleared-absent" {
							result, err := c.Commit(ctx, request)
							if err != nil {
								return result, err
							}
							if err := aterequest.VerifyAcknowledgement(original, result); err != nil {
								return result, err
							}
						}
						if mode == "mutated-executor" {
							request.Body[0] = '!'
						}
						return api.CommitResult{}, aterequest.ErrUnknown
					}), nil
				}})
			if err != nil {
				t.Fatal(err)
			}
			got, err := store.Append(t.Context(), "interleaved", session.AnySequence, pendingEvent("original"))
			if batches != 2 {
				t.Fatalf("expected exactly one fresh batch, got %d: %v", batches, err)
			}
			switch mode {
			case "cleared-ack", "mutated-executor", "different-pending":
				events, decodeErr := f.store.eventsFromRequest(original.Request, nil)
				if err != nil || decodeErr != nil || !reflect.DeepEqual(got, events) {
					t.Fatalf("original authoritative ACK not recovered: %v %v", err, decodeErr)
				}
			case "cleared-absent":
				if got != nil || !errors.Is(err, aterequest.ErrUnknown) || len(pending.rows) != 0 {
					t.Fatalf("missing pending resurrected or treated as ACK: %v", err)
				}
			case "changed-envelope":
				if got != nil || !errors.Is(err, aterequest.ErrConflict) {
					t.Fatalf("recovery accepted a changed original envelope: %v", err)
				}
			case "recovery-deadline":
				if got != nil || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, aterequest.ErrUnknown) {
					t.Fatalf("recovery exceeded its single bounded attempt: %v", err)
				}
				record, found, loadErr := pending.Get(t.Context(), original.Request.Key())
				if loadErr != nil || !found || !record.Attempted || !aterequest.Equal(record, original) {
					t.Fatal("recovery timeout lost original pending attempt")
				}
			}
			if mode == "different-pending" {
				record, found, err := pending.Get(t.Context(), unrelated.Request.Key())
				if err != nil || !found || record.Attempted || !sameRequest(record.Request, unrelated.Request) {
					t.Fatal("reconciliation touched a different pending request")
				}
			}
			if mode == "cleared-ack" && len(pending.rows) != 0 {
				t.Fatal("receipt recovery recreated cleared pending")
			}
			pending.mu.Lock()
			attempts := append([]api.RequestKey(nil), pending.attempts...)
			pending.mu.Unlock()
			if len(attempts) != 1 || attempts[0] != original.Request.Key() {
				t.Fatalf("recovery acquired a new attempt: %v", attempts)
			}
			loaded, err := f.store.Load(t.Context(), "interleaved", 1, 10)
			want := 1
			if mode == "cleared-absent" {
				want = 0
			}
			if err != nil || len(loaded) != want {
				t.Fatalf("recovery changed native history: %v", err)
			}
		})
	}
}

func TestReconciliationOptionAndPlanValidation(t *testing.T) {
	f := newNativeFixture(t)
	if _, err := New(Options{Process: f.process, Pending: f.pending, Space: f.store.space,
		StoreID: f.storeID, ReconciliationTimeout: -time.Second}); err == nil {
		t.Fatal("negative reconciliation timeout accepted")
	}
	f.store.reconciliationTimeout = time.Second
	events, err := makeEvents("validation", 0, []session.PendingEvent{largePending(400 << 10)})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := f.store.prepareAppend("validation", 0, events)
	if err != nil {
		t.Fatal(err)
	}
	plan.Request.RequestID += "-changed"
	if saved, err := f.store.saveAppend(t.Context(), plan, events); !errors.Is(err, ErrCorrupt) || saved != nil || len(f.pending.rows) != 0 {
		t.Fatalf("mismatched upload/append identity admitted: %v", err)
	}
}

type deadlineAppendInsert struct{ *pendingStore }

func (p deadlineAppendInsert) Insert(ctx context.Context, record aterequest.Record) error {
	if strings.HasPrefix(string(record.Request.RequestID), requestPrefix+"append-") {
		return context.DeadlineExceeded
	}
	return p.pendingStore.Insert(ctx, record)
}

func TestNativeAppendPartialPlanDoesNotReconcile(t *testing.T) {
	f := newNativeFixture(t)
	batches := 0
	store, err := New(Options{
		Process: processFunc(func(ctx context.Context, fn func(*client.Client) error) error {
			batches++
			return f.process.WithClient(ctx, fn)
		}),
		Pending: deadlineAppendInsert{f.pending}, Space: f.store.space, StoreID: f.storeID,
		ReconciliationTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.Append(t.Context(), "partial-plan", session.AnySequence, largePending(400<<10))
	if events != nil || !errors.Is(err, context.DeadlineExceeded) || batches != 1 {
		t.Fatalf("partial preparation started fresh recovery: batches=%d err=%v", batches, err)
	}
	records, err := aterequest.ListPending(t.Context(), f.pending)
	if err != nil || len(records) != 1 || records[0].Attempted || records[0].Request.Method != api.MethodMutateState {
		t.Fatalf("partially saved upload was attempted or lost: %v", err)
	}
}

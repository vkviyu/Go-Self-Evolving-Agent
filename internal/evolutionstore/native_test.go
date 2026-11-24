package evolutionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	ate "github.com/vkviyu/agent-temporal-engine"
	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/service"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/retention"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/strategy"
)

// The real engine, service and client enforce native durability and root CAS.
// Only the transport of a rejected commit is injected.
type nativeBackend struct {
	*ate.Store
	reject bool
}

func (b *nativeBackend) Commit(ctx context.Context, request api.CommitRequest) (api.CommitResult, error) {
	if b.reject {
		return api.CommitResult{RequestID: request.RequestID, Code: api.CodeRejected}, nil
	}
	return b.Store.Commit(ctx, request)
}

type nativeProcess struct {
	backend *nativeBackend
	mu      sync.Mutex
}

func (p *nativeProcess) WithClient(ctx context.Context, fn func(*client.Client) error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
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
	path    string
	backend *nativeBackend
	db      *runtimestore.Database
	store   *Store
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "native")
	must(t, os.Mkdir(path, 0700))
	engine, err := ate.Create(path, ate.Options{})
	must(t, err)
	f := &fixture{path: path, backend: &nativeBackend{Store: engine}}
	f.newRuntime(t)
	t.Cleanup(func() {
		must(t, f.db.Close())
		must(t, f.backend.Store.Close())
	})
	return f
}

func (f *fixture) newRuntime(t *testing.T) {
	t.Helper()
	// A completely new runtime, with no old business tables, can recover every
	// record. SQLite only hosts the ordinary ATE pending request envelopes.
	path := filepath.Join(t.TempDir(), "runtime.db")
	db, err := runtimestore.OpenRuntime(t.Context(), path)
	must(t, err)
	f.db = db
	probe, err := sql.Open("sqlite", path)
	must(t, err)
	for _, table := range []string{"strategy_specs", "strategy_suites", "strategy_reports", "strategy_active", "strategy_quarantine", "strategy_events", "plugin_builds", "plugin_test_reports"} {
		_, err := probe.ExecContext(t.Context(), "DROP TABLE IF EXISTS "+table)
		must(t, err)
	}
	must(t, probe.Close())
	authority, err := ateauthority.New(ateauthority.Options{
		Process: &nativeProcess{backend: f.backend}, Pending: db.ATERequests(),
		Space: "evolution-test", Principal: "gsea",
	})
	must(t, err)
	f.store, err = New(authority)
	must(t, err)
}

func (f *fixture) reopen(t *testing.T) {
	t.Helper()
	must(t, f.db.Close())
	must(t, f.backend.Store.Close())
	engine, report, err := ate.OpenExisting(f.path, ate.Options{})
	must(t, err)
	f.backend.Store = engine
	if !report.Verified || !report.PublicationSynced {
		t.Fatal("native authority was not verified on reopen")
	}
	f.newRuntime(t)
}

func strategyReport(t *testing.T, s *Store, revision string) (strategy.Spec, strategy.Report, string) {
	t.Helper()
	ctx := t.Context()
	// No Session is fabricated for host/CLI operations.
	spec := strategy.Spec{Version: 1, Revision: revision, InstructionSuffix: "Use explicit evidence"}
	id, err := s.Save(ctx, spec)
	must(t, err)
	suite := strategy.Suite{Name: "trusted", Fixtures: []strategy.Fixture{
		{Name: "safety", Prompt: "Do not call tools", Expected: "OK", Safety: true},
		{Name: "accuracy", Prompt: "1+1", Expected: "2"},
	}}
	suiteID, err := s.SaveSuite(ctx, suite, true)
	must(t, err)
	results := []strategy.Outcome{
		{Fixture: "safety", Safety: true, Passed: true, ModelCalls: 1},
		{Fixture: "accuracy", Passed: true, ModelCalls: 1},
	}
	report := strategy.Report{CandidateID: id, BaselineConfigID: "base", SuiteID: suiteID,
		EvaluatedAt: time.Now().UTC(), Baseline: append([]strategy.Outcome(nil), results...),
		Candidate: append([]strategy.Outcome(nil), results...)}
	reportID, err := s.SaveReport(ctx, report)
	must(t, err)
	return spec, report, reportID
}

func events(t *testing.T, s *Store) []strategyEvent {
	t.Helper()
	var out []strategyEvent
	must(t, s.authority.View(t.Context(), func(tx *ateauthority.Tx) error {
		return tx.List(recordKey("strategy-event")+"/", func(_ string, raw json.RawMessage) error {
			var event strategyEvent
			if err := json.Unmarshal(raw, &event); err != nil {
				return err
			}
			if event.Checksum != strategy.Digest(event.Payload) || event.OccurredAt.IsZero() {
				t.Fatal("invalid event evidence")
			}
			out = append(out, event)
			return nil
		})
	}))
	return out
}

func TestNativeStrategiesPublicationFallbackAndFreshRuntime(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	spec, report, id := strategyReport(t, f.store, "v1")
	if err := f.store.Promote(ctx, id, false); err == nil {
		t.Fatal("published without consent")
	}
	must(t, f.store.Promote(ctx, id, true))
	must(t, f.store.Promote(ctx, id, true))
	if got := events(t, f.store); len(got) != 1 || got[0].Type != "strategy.promoted" {
		t.Fatalf("publication was not idempotent: %+v", got)
	}
	f.reopen(t)
	active, err := f.store.Active(ctx, "base")
	must(t, err)
	if active == nil || active.ID() != spec.ID() || active.OwnerSession != "" {
		t.Fatalf("lost CLI publication or fabricated session: %+v", active)
	}
	if other, err := f.store.Active(ctx, "other"); err != nil || other != nil {
		t.Fatal("activation leaked across baselines", err)
	}
	if _, err := f.store.Suite(ctx, report.SuiteID); err != nil {
		t.Fatal("lost suite", err)
	}
	if _, err := f.store.Report(ctx, id); err != nil {
		t.Fatal("lost report", err)
	}
	if repeated, err := f.store.Save(ctx, spec); err != nil || repeated != spec.ID() {
		t.Fatal("idempotent spec save", err)
	}
	changed := spec
	changed.InstructionSuffix = "rewritten"
	if _, err := f.store.Save(ctx, changed); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatal("rebound owner/revision", err)
	}
	changed.OwnerSession = "other-owner"
	if _, err := f.store.Save(ctx, changed); err != nil {
		t.Fatal("revision uniqueness was not owner-scoped", err)
	}
	_, next, stale := strategyReport(t, f.store, "v2")
	if err := f.store.Promote(ctx, stale, true); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatal("accepted stale baseline", err)
	}
	next.PreviousSpecID = spec.ID()
	fresh, err := f.store.SaveReport(ctx, next)
	must(t, err)
	must(t, f.store.Promote(ctx, fresh, true))
	must(t, f.store.Fallback(ctx, "base", spec.ID(), "old failure"))
	active, err = f.store.Active(ctx, "base")
	must(t, err)
	if active == nil || active.ID() != next.CandidateID {
		t.Fatal("old failure removed newer publication")
	}
	must(t, f.store.Fallback(ctx, "base", next.CandidateID, "failed turn"))
	must(t, f.store.Fallback(ctx, "base", next.CandidateID, "duplicate"))
	f.reopen(t)
	active, err = f.store.Active(ctx, "base")
	if err != nil || active != nil {
		t.Fatal("fallback did not survive fresh runtime", err)
	}
	if err := f.store.Promote(ctx, fresh, true); err == nil {
		t.Fatal("quarantine lost on reopen")
	}
	audit := events(t, f.store)
	if len(audit) != 3 || audit[2].Type != "strategy.fallback" || audit[2].Payload["reason"] != "failed turn" {
		t.Fatalf("incorrect publication/fallback audit: %+v", audit)
	}
	for i, event := range audit {
		if event.Sequence != uint64(i+1) {
			t.Fatal("event order lost")
		}
	}
	// Each spec is independently addressable in native state, not an aggregate
	// JSON database snapshot.
	record, err := f.backend.GetState(ctx, api.ReadStateRequest{
		Space: "evolution-test", Key: []byte(recordKey("strategy-spec", spec.ID())),
	})
	must(t, err)
	if !record.Found || record.Value.Kind != api.ValueBytes {
		t.Fatal("missing individual native strategy record")
	}
	var doc document
	must(t, json.Unmarshal(record.Value.Data, &doc))
	if retention.Digest(doc.Data) != spec.ID() {
		t.Fatal("native spec digest differs")
	}
}

func TestNativeStrategyGatesAndAtomicPublication(t *testing.T) {
	for _, mode := range []string{"safety", "regression", "cost", "zero-calls", "duplicate", "missing", "wrong-suite", "wrong-safety", "missing-previous", "tamper", "commit-rejected", "event-collision"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			ctx := t.Context()
			_, report, id := strategyReport(t, f.store, "v1")
			switch mode {
			case "safety":
				report.Candidate[0].Passed = false
			case "regression":
				report.Candidate[1].Passed = false
			case "cost":
				report.Candidate[1].ModelCalls = 2
			case "zero-calls":
				report.Candidate[0].ModelCalls = 0
			case "duplicate":
				report.Candidate[1], report.Baseline[1] = report.Candidate[0], report.Baseline[0]
			case "missing":
				report.Candidate, report.Baseline = report.Candidate[:1], report.Baseline[:1]
			case "wrong-suite":
				report.SuiteID = "unknown"
			case "wrong-safety":
				report.Candidate[0].Safety, report.Baseline[0].Safety = false, false
			case "missing-previous":
				report.PreviousSpecID = "unknown"
			case "tamper":
				must(t, f.store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
					return tx.Put(recordKey("strategy-report", id), document{Data: []byte("{}"), Digest: id})
				}))
			case "commit-rejected":
				f.backend.reject = true
			case "event-collision":
				must(t, f.store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
					return tx.Create(recordKey("strategy-event")+"/00000000000000000001", strategyEvent{})
				}))
			}
			if mode != "tamper" && mode != "commit-rejected" && mode != "event-collision" {
				var err error
				id, err = f.store.SaveReport(ctx, report)
				if err != nil {
					return
				}
			}
			err := f.store.Promote(ctx, id, true)
			if err == nil {
				t.Fatal("unsafe or failed publication succeeded")
			}
			if mode == "commit-rejected" && !errors.Is(err, strategy.ErrEvidence) {
				t.Fatal("lost fatal evidence classification", err)
			}
			f.backend.reject = false
			active, err := f.store.Active(ctx, "base")
			if err != nil || active != nil {
				t.Fatal("failed publication changed pointer", err)
			}
			if mode != "event-collision" && len(events(t, f.store)) != 0 {
				t.Fatal("failed publication left an event")
			}
		})
	}
}

func TestNativeStrategyFallbackAtomicFailureAndActiveEvidence(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	spec, _, reportID := strategyReport(t, f.store, "v1")
	must(t, f.store.Promote(ctx, reportID, true))
	f.backend.reject = true
	if err := f.store.Fallback(ctx, "base", spec.ID(), "failed"); !errors.Is(err, strategy.ErrEvidence) {
		t.Fatal("expected fatal commit failure", err)
	}
	f.backend.reject = false
	active, err := f.store.Active(ctx, "base")
	if err != nil || active == nil || active.ID() != spec.ID() || len(events(t, f.store)) != 1 {
		t.Fatal("rejected fallback changed publication or quarantine", err)
	}
	other, _, _ := strategyReport(t, f.store, "unpublished")
	must(t, f.store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		return tx.Put(recordKey("strategy-active", "base"), activation{SpecID: other.ID(), ReportID: reportID})
	}))
	if _, err := f.store.Active(ctx, "base"); err == nil {
		t.Fatal("active pointer tampering bypassed publication evidence")
	}
}

func TestNativeStrategyConsentAndImmutableChecksums(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	spec, report, _ := strategyReport(t, f.store, "v1")
	suite, err := f.store.Suite(ctx, report.SuiteID)
	must(t, err)
	if _, err := f.store.SaveSuite(ctx, suite, false); err == nil {
		t.Fatal("registered expectations without confirmation")
	}
	for _, entity := range []struct {
		kind string
		id   string
	}{{"strategy-spec", spec.ID()}, {"strategy-suite", report.SuiteID}} {
		must(t, f.store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
			var doc document
			if _, err := tx.Get(recordKey(entity.kind, entity.id), &doc); err != nil {
				return err
			}
			doc.Data = append(doc.Data, ' ')
			return tx.Put(recordKey(entity.kind, entity.id), doc)
		}))
	}
	if _, err := f.store.Load(ctx, spec.ID()); err == nil {
		t.Fatal("spec read masked corruption")
	}
	if _, err := f.store.Save(ctx, spec); err == nil {
		t.Fatal("spec save masked corruption")
	}
	if _, err := f.store.Suite(ctx, report.SuiteID); err == nil {
		t.Fatal("suite read masked corruption")
	}
	if _, err := f.store.SaveSuite(ctx, suite, true); err == nil {
		t.Fatal("suite save masked corruption")
	}
}

func TestNativeConcurrentPublication(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	_, _, first := strategyReport(t, f.store, "first")
	_, _, second := strategyReport(t, f.store, "second")
	ready := make(chan struct{})
	results := make(chan error, 2)
	for _, id := range []string{first, second} {
		go func() {
			<-ready
			results <- f.store.Promote(ctx, id, true)
		}()
	}
	close(ready)
	succeeded, conflicted := 0, 0
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, orchestration.ErrIdempotencyConflict):
			conflicted++
		default:
			t.Fatal(err)
		}
	}
	if succeeded != 1 || conflicted != 1 || len(events(t, f.store)) != 1 {
		t.Fatal("competing publications did not compare the exact baseline")
	}
}

func TestNativeStrategyEventSequenceScanOrder(t *testing.T) {
	f := newFixture(t)
	must(t, f.store.authority.Update(t.Context(), func(tx *ateauthority.Tx) error {
		if err := tx.Put(recordKey("strategy-event-sequence"), uint64(398)); err != nil {
			return err
		}
		for range 4 {
			if err := appendStrategyEvent(tx, "strategy.promoted", map[string]string{"baseline_id": "base"}); err != nil {
				return err
			}
		}
		return nil
	}))
	got := events(t, f.store)
	if len(got) != 4 {
		t.Fatalf("missing events: %+v", got)
	}
	for i, event := range got {
		if event.Sequence != 399+uint64(i) {
			t.Fatalf("native scan reordered event sequences: %+v", got)
		}
	}
}

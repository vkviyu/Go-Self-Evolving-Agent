package experience

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"
	ate "github.com/vkviyu/agent-temporal-engine"
	ateagentprofile "github.com/vkviyu/agent-temporal-engine/agentprofile"
	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/memory"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/agent-temporal-engine/service"
	"github.com/vkviyu/agent-temporal-engine/spacecatalog"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/atebridge"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateports"
	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/atestore"
)

type authorityTestState struct{ store *ateauthority.Store }

func (s authorityTestState) Get(ctx context.Context, key string) (json.RawMessage, bool, error) {
	var raw json.RawMessage
	var found bool
	err := s.store.View(ctx, func(tx *ateauthority.Tx) error {
		var err error
		found, err = tx.Get(key, &raw)
		return err
	})
	return raw, found, err
}

func (s authorityTestState) Put(ctx context.Context, key string, value json.RawMessage) error {
	return s.store.Update(ctx, func(tx *ateauthority.Tx) error {
		var prior json.RawMessage
		found, err := tx.Get(key, &prior)
		if err != nil {
			return err
		}
		if found {
			if !bytes.Equal(prior, value) {
				return ateports.ErrConflict
			}
			return nil
		}
		return tx.Create(key, value)
	})
}

type nativeTestProcess struct{ p *testProcess }

type changedNativeHistory struct{ *atestore.Store }

func (s changedNativeHistory) HistoryThrough(ctx context.Context, id string, after, through uint64, limit int) ([]ateports.Record, error) {
	records, err := s.Store.HistoryThrough(ctx, id, after, through, limit)
	for i := range records {
		records[i].Payload = bytes.ReplaceAll(records[i].Payload, []byte("EARLY"), []byte("CHANGED"))
	}
	return records, err
}

func (p nativeTestProcess) WithClient(ctx context.Context, fn func(*client.Client) error) error {
	if p.p.leased.Load() {
		return errors.New("nested native process lease")
	}
	return p.p.WithClient(ctx, fn)
}

func TestNativeAuthorityExperienceUsesOriginalSessionAnchors(t *testing.T) {
	const originalMarker = "original-payload-must-not-appear-in-experience-state-726418"
	f := newFixture(t)
	process := nativeTestProcess{f.p}
	authority, err := ateauthority.New(ateauthority.Options{
		Process: process, Pending: f.db.ATERequests(), Space: "native-decisions", Principal: "gsea",
	})
	if err != nil {
		t.Fatal(err)
	}
	state := authorityTestState{authority}
	f.b, err = atebridge.New(atebridge.Options{
		Process: process, Pending: f.db.ATERequests(), State: state, Namespace: "gsea:test-native-experience",
	})
	if err != nil {
		t.Fatal(err)
	}
	var storeID model.StoreID
	if err := f.b.WithClient(t.Context(), func(c *client.Client) error {
		snapshot, err := c.Snapshot(t.Context())
		storeID = snapshot.Fence.Store
		return err
	}); err != nil {
		t.Fatal(err)
	}
	history, err := atestore.New(atestore.Options{
		Process: process, Pending: f.db.ATERequests(), Space: "gsea-authority", StoreID: storeID,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, incomplete := range []HistoryReader{
		struct{ HistoryReader }{history},
		struct {
			HistoryReader
			EvidenceSourceProvider
		}{history, history},
	} {
		if _, err := New(Options{Bridge: f.b, State: state, History: incomplete}); err == nil {
			t.Fatal("native experience accepted missing evidence/cut capability")
		}
	}
	options := Options{Bridge: f.b, State: state, History: history}
	f.s, err = New(options)
	if err != nil {
		t.Fatal(err)
	}
	m := newModel(t, f)
	if _, err := f.s.Prepare(t.Context(), "native-session", "first", "Recover a failed request", m.generate); err != nil {
		t.Fatal(err)
	}
	_, err = history.Append(t.Context(), "native-session", 0, session.PendingEvent{
		Type: session.EventToolSucceeded, Payload: session.MustJSONPayload(map[string]string{
			"text": "EARLY: bounded retry recovered a transient failure", "raw_detail": originalMarker,
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.Complete(t.Context(), "native-session", "first", m.generate); err != nil {
		t.Fatal(err)
	}
	m.full = true
	value, err := f.s.Prepare(t.Context(), "native-session", "second", "Reuse bounded retry", m.generate)
	if err != nil || len(value.Memories) != 1 || !value.HistoryComplete || len(value.EvidenceReferences) != 1 {
		t.Fatalf("native selection/history: %+v %v", value, err)
	}
	if err := f.s.Check(t.Context(), value); err != nil {
		t.Fatal(err)
	}
	if value.Sources[len(value.Memories)].Anchor.Space != "gsea-authority" {
		t.Fatal("original session evidence was reprojected into bridge space")
	}
	// Reopen the real ATE store. The service has no SQL decisions or evidence.
	if err := f.p.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.p.store, _, err = ate.OpenExisting(f.path, ate.Options{AgentProfile: true})
	if err != nil {
		t.Fatal(err)
	}
	f.s, err = New(options)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := f.s.Prepare(t.Context(), "native-session", "second", "Reuse bounded retry", nil)
	if err != nil || !reflect.DeepEqual(value, replayed) {
		t.Fatalf("native durable decisions changed on restart: %v", err)
	}
	if err := f.s.Check(t.Context(), replayed); err != nil {
		t.Fatal(err)
	}
	pending, err := f.db.ATERequests().List(t.Context(), aterequest.ListOptions{Limit: 128, MaxBytes: aterequest.MaxListBytes})
	if err != nil || len(pending) != 0 {
		t.Fatalf("completed native commands retained local bodies: %v", err)
	}
	// Frozen history is rebuilt from its original authority, even after later
	// appends, and checked against the saved coverage/checksum.
	if _, err := history.Append(t.Context(), "native-session", 1, session.PendingEvent{
		Type: session.EventUserMessage, Payload: json.RawMessage(`{"content":"later source event"}`),
	}); err != nil {
		t.Fatal(err)
	}
	frozenKey := f.s.key("native-session", "first") + "/complete/history"
	frozen, err := f.s.originalHistory(t.Context(), frozenKey, "native-session")
	if err != nil || frozen.Through != 1 || len(frozen.Events) != 1 ||
		!bytes.Contains(frozen.Events[0].Payload, []byte(originalMarker)) {
		t.Fatalf("native history was not reconstructed at the original cut: %v", err)
	}
	alteredOptions := options
	alteredOptions.History = changedNativeHistory{history}
	altered, err := New(alteredOptions)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := altered.originalHistory(t.Context(), frozenKey, "native-session"); !errors.Is(err, ErrSelection) {
		t.Fatalf("modified frozen source accepted: %v", err)
	}
	for _, stage := range []string{"history-map", "verify"} {
		t.Run(stage+" retry", func(t *testing.T) {
			key := "native-input-retry/" + stage
			input := map[string]string{"original": originalMarker}
			var output notes
			calls := 0
			interrupted := errors.New("model interrupted")
			generate := func(_ context.Context, _ []*schema.Message) (*schema.Message, error) {
				calls++
				if calls == 1 {
					return nil, interrupted
				}
				return schema.AssistantMessage(`{"notes":[]}`, nil), nil
			}
			validate := func() error { return nil }
			if err := f.s.decide(t.Context(), key, stage, notesSchema, "", input, generate, &output, validate); !errors.Is(err, interrupted) {
				t.Fatal(err)
			}
			input["original"] = "changed input"
			if err := f.s.decide(t.Context(), key, stage, notesSchema, "", input, generate, &output, validate); !errors.Is(err, ateports.ErrConflict) || calls != 1 {
				t.Fatalf("changed evidence reached model on retry: %v", err)
			}
			input["original"] = originalMarker
			if err := f.s.decide(t.Context(), key, stage, notesSchema, "", input, generate, &output, validate); err != nil || calls != 2 {
				t.Fatalf("identical source input could not resume: %v", err)
			}
		})
	}
	// Inspect every durable decision value, including model input checkpoints.
	// Derived notes/claims are retained; original event text and wrappers are not.
	snapshots, inputDigests := 0, 0
	if err := authority.View(t.Context(), func(tx *ateauthority.Tx) error {
		return tx.List("", func(key string, raw json.RawMessage) error {
			if bytes.Contains(raw, []byte(originalMarker)) || bytes.Contains(raw, []byte(`"OriginalEvents"`)) ||
				bytes.Contains(raw, []byte(`"original_json"`)) {
				t.Fatalf("source history duplicated in %s", key)
			}
			if strings.HasSuffix(key, "/history") {
				var saved struct {
					Events         []json.RawMessage
					ReferencesOnly bool
					EventsSHA256   string
				}
				if err := json.Unmarshal(raw, &saved); err != nil {
					return err
				}
				if len(saved.Events) != 0 || !saved.ReferencesOnly || saved.EventsSHA256 == "" {
					t.Fatalf("history checkpoint contains payloads: %s", key)
				}
				snapshots++
			}
			if strings.HasSuffix(key, "/input-digest") {
				inputDigests++
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	if snapshots < 2 || inputDigests < 3 {
		t.Fatal("history/model checkpoints were not exercised")
	}
	if err := f.s.Check(t.Context(), replayed); err != nil {
		t.Fatalf("later source fence invalidated original provenance: %v", err)
	}
	source := value.Sources[len(value.Memories)]
	if err := f.s.applyMemory(t.Context(), "native-withdrawal", source.Anchor.Space, memory.Command{
		RetractSource: &memory.RetractSourceCommand{Source: source.SourceID,
			Retraction: memory.Retraction{Kind: memory.RetractionUseBan, Reason: "source withdrawn"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Check(t.Context(), replayed); err == nil {
		t.Fatal("pinned native provenance bypassed current source withdrawal")
	}
}

type (
	testProcess struct {
		mu         sync.Mutex
		leased     atomic.Bool
		store      *ate.Store
		dropMethod string
		commits    int
		leases     int
	}
	testBackend struct {
		*ate.Store
		p    *testProcess
		conn net.Conn
	}
	fixture struct {
		s       *Service
		b       *atebridge.Bridge
		p       *testProcess
		db      *runtimestore.Database
		path    string
		state   authorityTestState
		history *atestore.Store
	}
	fakeModel struct {
		t         *testing.T
		p         *testProcess
		calls     map[string]int
		full      bool
		reject    bool
		fabricate bool
		organize  bool
		seen      map[string]bool
	}
)

func (b *testBackend) Commit(ctx context.Context, r api.CommitRequest) (api.CommitResult, error) {
	// commits counts durable BUSINESS mutations, so tests can assert that
	// decisions, pinned inputs and session history are not business writes.
	if r.Method != api.MethodMutateState && r.Method != api.MethodApplyAgent && r.Method != api.MethodApplyWork {
		b.p.commits++
	}
	result, err := b.Store.Commit(ctx, r)
	if r.Method == b.p.dropMethod {
		b.p.dropMethod = ""
		_ = b.conn.Close()
	}
	return result, err
}

func (p *testProcess) WithClient(ctx context.Context, fn func(*client.Client) error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.leases++
	if p.leased.Swap(true) {
		panic("nested process lease")
	}
	defer p.leased.Store(false)
	a, z := net.Pipe()
	server, err := service.New(&testBackend{Store: p.store, p: p, conn: z}, service.Options{
		Credentials: map[string]service.Credential{"test": {Principal: "gsea", AllSpaces: true, AllowGlobalSnapshot: true}},
	})
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- server.ServeStream(ctx, z) }()
	defer func() { _ = a.Close(); _ = server.Shutdown(context.Background()); <-done }()
	c, err := client.New(ctx, a, client.Options{Token: "test"})
	if err != nil {
		return err
	}
	defer c.Close()
	return fn(c)
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := t.Context()
	f := &fixture{path: t.TempDir()}
	store, err := ate.Create(f.path, ate.Options{AgentProfile: true})
	if err != nil {
		t.Fatal(err)
	}
	f.p = &testProcess{store: store}
	f.db, err = runtimestore.OpenRuntime(ctx, filepath.Join(t.TempDir(), "session.db"))
	if err != nil {
		t.Fatal(err)
	}
	process := nativeTestProcess{f.p}
	authority, err := ateauthority.New(ateauthority.Options{Process: process, Pending: f.db.ATERequests(), Space: "native-decisions", Principal: "gsea"})
	if err != nil {
		t.Fatal(err)
	}
	f.state = authorityTestState{authority}
	f.b, err = atebridge.New(atebridge.Options{Process: process, Pending: f.db.ATERequests(), State: f.state, Namespace: digest(f.path)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.b.ExecuteInSpace(ctx, "gsea-authority", "session-space", func(*client.Client) (api.CommitRequest, error) {
		body, err := spacecatalog.EncodeApply(spacecatalog.Apply{Name: "Original sessions"})
		return api.CommitRequest{Space: "gsea-authority", Method: api.MethodApplySpace, Body: body}, err
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.history, err = atestore.New(atestore.Options{Process: process, Pending: f.db.ATERequests(), Space: "gsea-authority", StoreID: snapshot.Fence.Store})
	if err != nil {
		t.Fatal(err)
	}
	f.s, err = New(Options{Bridge: f.b, State: f.state, History: f.history})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.db.Close(); _ = f.p.store.Close() })
	return f
}

func (f *fixture) restart(t *testing.T) {
	t.Helper()
	if err := f.p.store.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.p.store, _, err = ate.OpenExisting(f.path, ate.Options{AgentProfile: true})
	if err != nil {
		t.Fatal(err)
	}
	f.s, err = New(Options{Bridge: f.b, State: f.state, History: f.history})
	if err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) events(t *testing.T, id string, count int) {
	t.Helper()
	events := make([]session.PendingEvent, count)
	for i := range events {
		text := strings.Repeat("unrelated filler ", 200)
		if i == 0 {
			text = "EARLY: tool succeeded: bounded retry recovered a transient failure; project=/projects/alpha"
		}
		if i == count-1 && i > 0 {
			text = "LATE: tool succeeded: bounded retry also recovered a network request; project=/projects/alpha"
		}
		raw, _ := json.Marshal(map[string]any{"text": text})
		events[i] = session.PendingEvent{Type: session.EventToolSucceeded, Payload: raw, CorrelationID: "turn"}
	}
	appendBounded(t, f.history, session.ID(id), events)
}

// appendBounded respects the native append's per-command event bound. Atomicity
// is per chunk; callers that need a single atomic batch must stay under it.
func appendBounded(t *testing.T, history *atestore.Store, id session.ID, events []session.PendingEvent) {
	t.Helper()
	for start := 0; start < len(events); start += ateagentprofile.MaxSessionAppendEvents {
		end := min(start+ateagentprofile.MaxSessionAppendEvents, len(events))
		if _, err := history.Append(t.Context(), id, session.AnySequence, events[start:end]...); err != nil {
			t.Fatal(err)
		}
	}
}

func newModel(t *testing.T, f *fixture) *fakeModel {
	return &fakeModel{t: t, p: f.p, calls: map[string]int{}, seen: map[string]bool{}}
}

func (m *fakeModel) generate(ctx context.Context, messages []*schema.Message) (*schema.Message, error) {
	m.t.Helper()
	if m.p.leased.Load() {
		m.t.Fatal("model called while process lease held")
	}
	if len(messages) != 2 || messages[1].Role != schema.User {
		m.t.Fatal("unexpected model input roles")
	}
	stage := strings.Split(strings.Split(messages[0].Content, "Stage: ")[1], "\n")[0]
	m.calls[stage]++
	var result any
	switch stage {
	case "discover":
		var in struct {
			Goal      string
			Directory directory
		}
		if err := json.Unmarshal([]byte(messages[1].Content), &in); err != nil {
			m.t.Fatal(err)
		}
		r := route{SpaceIDs: []string{}, SearchTerms: []string{"retry"}, FullHistory: m.full}
		if len(in.Directory.Spaces) == 0 {
			r.NewSpace = &newSpace{Name: "Resilience patterns", Description: "Transferable recovery and bounded retry lessons across domains"}
		} else {
			r.SpaceIDs = append(r.SpaceIDs, string(in.Directory.Spaces[0].ID))
		}
		result = r
	case "history-map":
		var fragments []fragment
		if err := json.Unmarshal([]byte(messages[1].Content), &fragments); err != nil {
			m.t.Fatal(err)
		}
		n := notes{Notes: []note{}}
		for _, f := range fragments {
			m.seen[f.EventID] = true
			for _, marker := range []string{"EARLY", "LATE"} {
				if strings.Contains(f.OriginalJSON, marker) {
					n.Notes = append(n.Notes, note{Text: marker + ": bounded retry recovered a recorded transient failure", EventIDs: []string{f.EventID}})
				}
			}
		}
		result = n
	case "history-reduce":
		var in []note
		if err := json.Unmarshal([]byte(messages[1].Content), &in); err != nil {
			m.t.Fatal(err)
		}
		result = notes{Notes: in[:min(12, len(in))]}
	case "extract":
		var n notes
		if err := json.Unmarshal([]byte(messages[1].Content), &n); err != nil {
			m.t.Fatal(err)
		}
		e := extraction{Lessons: []lesson{}}
		if len(n.Notes) > 0 {
			ids := []string{}
			for _, v := range n.Notes {
				ids = append(ids, v.EventIDs...)
			}
			if m.fabricate {
				ids = []string{"invented-event"}
			}
			e.Lessons = append(e.Lessons, lesson{Claim: "Use bounded retry for transient failures; the observed recovery is not a guarantee.", Kind: "procedure", EventIDs: ids, Tags: []string{"retry"}})
		}
		result = e
	case "verify":
		var in reviewInput
		if err := json.Unmarshal([]byte(messages[1].Content), &in); err != nil {
			m.t.Fatal(err)
		}
		if len(in.OriginalEvents) == 0 || !strings.Contains(string(in.OriginalEvents[0].Payload), "EARLY") {
			m.t.Fatal("review did not receive original evidence")
		}
		result = review{Passed: !m.reject, Reason: "Independent model evidence review, not tool proof", EventIDs: []string{in.OriginalEvents[0].SourceID}, ClaimsToolSuccess: true, ToolSuccessEventIDs: []string{in.OriginalEvents[0].SourceID}}
	case "select":
		var in struct{ Catalog catalog }
		if err := json.Unmarshal([]byte(messages[1].Content), &in); err != nil {
			m.t.Fatal(err)
		}
		if !strings.Contains(in.Catalog.Memories[0].ClaimText, "bounded retry") {
			m.t.Fatal("selection model did not receive readable claim text")
		}
		result = selectionDecision{MemoryIDs: []string{in.Catalog.Memories[0].ID}}
	case "organize":
		var cat organizationInput
		if err := json.Unmarshal([]byte(messages[1].Content), &cat); err != nil {
			m.t.Fatal(err)
		}
		o := organization{Actions: []action{}}
		if m.organize && len(cat.Memories) > 0 {
			o.Actions = append(o.Actions, action{Kind: "rename", SpaceID: cat.Spaces[0].SpaceID, Name: "Cross-domain recovery", Description: "Reusable lessons across projects"})
			o.Actions = append(o.Actions, action{Kind: "attach", SpaceID: cat.Spaces[0].SpaceID, SourceID: cat.Memories[0].SourceID, Description: "A grounded retry lesson"})
		}
		result = o
	default:
		return nil, fmt.Errorf("unexpected stage %s", stage)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return schema.AssistantMessage(string(raw), nil), nil
}

func TestNativeEmptyCreatePromoteReuseAndReplay(t *testing.T) {
	f := newFixture(t)
	m := newModel(t, f)
	first, err := f.s.Prepare(t.Context(), "project-alpha-session", "turn-a", "Recover a failed network request", m.generate)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Memories) != 0 || first.Message().Role != schema.User {
		t.Fatalf("unexpected initial context: %+v", first)
	}
	f.events(t, "project-alpha-session", 2)
	m.organize = true
	if err = f.s.Complete(t.Context(), "project-alpha-session", "turn-a", m.generate); err != nil {
		t.Fatal(err)
	}
	second, err := f.s.Prepare(t.Context(), "project-beta-session", "turn-b", "Retry deployment in /projects/beta", m.generate)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Memories) != 1 || !strings.Contains(second.Text, "bounded retry") {
		t.Fatalf("cross-session reuse missing: %+v", second)
	}
	if second.Memories[0].Source.Anchor.Space == f.b.OperationalSpace() {
		t.Fatal("semantic memory placed in operational space")
	}
	if err = f.s.Check(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	counts := fmt.Sprint(m.calls)
	f.restart(t)
	again, err := f.s.Prepare(t.Context(), "project-beta-session", "turn-b", "Retry deployment in /projects/beta", nil)
	if err != nil || !reflect.DeepEqual(second, again) {
		t.Fatalf("replay changed selection: %v", err)
	}
	if err = f.s.Complete(t.Context(), "project-alpha-session", "turn-a", nil); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(m.calls) != counts {
		t.Fatal("replay called model")
	}
	second.Text += " forged"
	if !errors.Is(f.s.Check(t.Context(), second), ErrSelection) {
		t.Fatal("modified pending context accepted")
	}
}

func TestCompleteOriginalLongHistoryAndSemanticSummary(t *testing.T) {
	f := newFixture(t)
	m := newModel(t, f)
	f.events(t, "long", 150)
	if _, err := f.history.Append(t.Context(), "long", session.AnySequence, session.PendingEvent{Type: session.EventContextCompacted, Payload: json.RawMessage(`{"summary":"recent compressed summary omits earlier tool evidence"}`)}); err != nil {
		t.Fatal(err)
	}
	// Completed native commands leave no pending bodies, but all original
	// evidence must still reach the summary, including events before compaction.
	pending, err := f.db.ATERequests().List(t.Context(), aterequest.ListOptions{Limit: 128, MaxBytes: aterequest.MaxListBytes})
	if err != nil || len(pending) != 0 {
		t.Fatalf("native append left pending commands: %v", err)
	}
	if err := f.s.Complete(t.Context(), "long", "completed", m.generate); err != nil {
		t.Fatal(err)
	}
	if len(m.seen) != 150 || m.calls["verify"] != 1 {
		t.Fatalf("completion skipped original evidence: mapped=%d calls=%v", len(m.seen), m.calls)
	}
	m.seen = map[string]bool{}
	mapCalls := m.calls["history-map"]
	m.full = true
	value, err := f.s.Prepare(t.Context(), "long", "summary", "Summarize recovery across the full history", m.generate)
	if err != nil {
		t.Fatal(err)
	}
	if !value.HistoryComplete || value.HistoryCoverage == nil ||
		value.HistoryCoverage.Cut != 151 || value.HistoryCoverage.SourceEvents != 151 ||
		value.HistoryCoverage.PrimaryEvents != 150 ||
		value.HistoryCoverage.ExcludedNonprimary["derived_context_projection"] != 1 {
		t.Fatalf("incomplete long history: %+v", value.HistoryCoverage)
	}
	if len(m.seen) != 150 || m.calls["history-map"]-mapCalls < 2 || len(value.Memories) != 1 ||
		!strings.Contains(value.Text, "EARLY") || !strings.Contains(value.Text, "LATE") ||
		strings.Contains(value.Text, "unrelated filler") || len(value.EvidenceReferences) != 2 {
		t.Fatalf("full-history semantic summary lost evidence: mapped=%d calls=%v context=%s", len(m.seen), m.calls, value.Text)
	}
	if err := f.s.Check(t.Context(), value); err != nil {
		t.Fatal(err)
	}
	f.restart(t)
	again, err := f.s.Prepare(t.Context(), "long", "summary", "Summarize recovery across the full history", nil)
	if err != nil || !reflect.DeepEqual(value, again) {
		t.Fatalf("full-history summary changed on replay: %v", err)
	}
	if err := f.s.Complete(t.Context(), "long", "completed", nil); err != nil {
		t.Fatalf("long-history completion did not replay: %v", err)
	}
}

func TestOriginalSourceRetractionBlocksSelectionAndReplay(t *testing.T) {
	for _, kind := range []memory.RetractionKind{memory.RetractionUseBan, memory.RetractionPrivacyErasure, memory.RetractionACLRevocation} {
		t.Run(string(kind), func(t *testing.T) {
			f := newFixture(t)
			m := newModel(t, f)
			f.events(t, "origin", 1)
			if err := f.s.Complete(t.Context(), "origin", "turn", m.generate); err != nil {
				t.Fatal(err)
			}
			value, err := f.s.Prepare(t.Context(), "other-project", "consumer", "retry", m.generate)
			if err != nil {
				t.Fatal(err)
			}
			if len(value.Memories) != 1 {
				t.Fatalf("expected one reusable memory, got %d", len(value.Memories))
			}
			var originals []memoryItem
			err = f.b.WithClient(t.Context(), func(c *client.Client) error {
				var err error
				originals, err = queryAll(t.Context(), c, api.QueryMemoryRequest{Spaces: []model.SpaceID{value.Memories[0].Source.Anchor.Space}})
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(originals) != 1 || len(originals[0].Memory.Sources) == 0 {
				t.Fatal("missing native original provenance")
			}
			source := originals[0].Memory.Sources[0]
			if err = f.s.applyMemory(t.Context(), "withdraw-"+string(kind), source.Anchor.Space, memory.Command{RetractSource: &memory.RetractSourceCommand{
				Source: source.SourceID, Retraction: memory.Retraction{Kind: kind, Reason: "actual original event withdrawn"},
			}}); err != nil {
				t.Fatal(err)
			}
			if err = f.s.Check(t.Context(), value); err == nil {
				t.Fatal("withdrawn source passed dispatch gate")
			}
			if _, err = f.s.Prepare(t.Context(), "other-project", "consumer", "retry", nil); err == nil {
				t.Fatal("withdrawn selection replayed")
			}
		})
	}
}

func TestUnsupportedLessonRetainedOnlyAsCandidate(t *testing.T) {
	f := newFixture(t)
	m := newModel(t, f)
	m.reject = true
	f.events(t, "unsupported", 1)
	if err := f.s.Complete(t.Context(), "unsupported", "turn", m.generate); err != nil {
		t.Fatal(err)
	}
	d, err := f.s.directory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	err = f.b.WithClient(t.Context(), func(c *client.Client) error {
		state, err := c.GetMemoryState(t.Context(), api.ReadMemoryStateRequest{Space: d.Spaces[0].ID})
		if err != nil {
			return err
		}
		if len(state.Snapshot.Memories) != 1 || state.Snapshot.Memories[0].Status != memory.StatusCandidate {
			t.Fatal("unsupported candidate promoted or discarded")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.s.Complete(t.Context(), "unsupported", "turn", nil); err != nil {
		t.Fatal(err)
	}
}

func TestFabricatedCitationRejectedBeforePublication(t *testing.T) {
	f := newFixture(t)
	m := newModel(t, f)
	m.fabricate = true
	f.events(t, "bad-citation", 1)
	if err := f.s.Complete(t.Context(), "bad-citation", "turn", m.generate); !errors.Is(err, ErrDecision) {
		t.Fatalf("fabricated citation: %v", err)
	}
	d, err := f.s.directory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Spaces) != 0 {
		t.Fatalf("invalid extraction mutated semantic directory: %+v", d.Spaces)
	}
}

func TestAdoptionUnknownOutcomeReplaysWithoutModel(t *testing.T) {
	f := newFixture(t)
	m := newModel(t, f)
	f.events(t, "source", 1)
	if err := f.s.Complete(t.Context(), "source", "turn", m.generate); err != nil {
		t.Fatal(err)
	}
	f.p.dropMethod = api.MethodApplyKnowledge
	if _, err := f.s.Prepare(t.Context(), "target", "turn", "retry", m.generate); err == nil {
		t.Fatal("injected disconnect did not fail")
	}
	f.restart(t)
	value, err := f.s.Prepare(t.Context(), "target", "turn", "retry", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(value.Memories) != 1 {
		t.Fatal("replay lost adoption")
	}
	if err = f.s.Check(t.Context(), value); err != nil {
		t.Fatal(err)
	}
}

func TestOrganizationUnknownOutcomePreservesDecisionAndCAS(t *testing.T) {
	f := newFixture(t)
	m := newModel(t, f)
	m.organize = true
	f.events(t, "source", 1)
	// The first organization edit is a rename; the second is membership.
	f.p.dropMethod = api.MethodOrganizeMemory
	if err := f.s.Complete(t.Context(), "source", "turn", m.generate); err == nil {
		t.Fatal("injected disconnect did not fail")
	}
	pendingReport, err := f.s.MaintenanceReport(t.Context(), "source", "turn")
	if err != nil || pendingReport.Complete || len(pendingReport.Actions) != 2 ||
		pendingReport.Actions[0].Status != "applied" || pendingReport.Actions[1].Status != "pending" {
		t.Fatalf("unknown action misreported: %+v %v", pendingReport, err)
	}
	f.restart(t)
	if err := f.s.Complete(t.Context(), "source", "turn", nil); err != nil {
		t.Fatal(err)
	}
	report, err := f.s.MaintenanceReport(t.Context(), "source", "turn")
	if err != nil || !report.Complete || report.Actions[1].Status != "applied" ||
		report.Actions[1].Operation != pendingReport.Actions[1].Operation {
		t.Fatalf("reconciliation changed action identity: %+v %v", report, err)
	}
	d, err := f.s.directory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Spaces) != 1 || d.Spaces[0].Name != "Cross-domain recovery" {
		t.Fatalf("organization replay: %+v", d)
	}
	err = f.b.WithClient(t.Context(), func(c *client.Client) error {
		p, err := c.ListSpaceMemories(t.Context(), api.ListSpaceMemoriesRequest{Space: d.Spaces[0].ID})
		if err == nil && (len(p.Items) != 1 || p.Items[0].Version != 1) {
			t.Fatal("duplicate attachment on replay")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCatalogPaginatesWithoutPreconfiguredCategories(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < maxPageItems+2; i++ {
		body, err := spacecatalog.EncodeApply(spacecatalog.Apply{Name: fmt.Sprintf("Domain %03d", i), Description: "User-defined domain"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.s.commit(t.Context(), fmt.Sprintf("space/%d", i), api.CommitRequest{Space: model.SpaceID(fmt.Sprintf("domain-%03d", i)), Method: api.MethodApplySpace, Body: body}); err != nil {
			t.Fatal(err)
		}
	}
	d, err := f.s.directory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Spaces) != maxPageItems+2 {
		t.Fatalf("directory truncated: %d", len(d.Spaces))
	}
}

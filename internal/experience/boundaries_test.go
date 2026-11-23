package experience

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/memory"
	"github.com/vkviyu/go-self-evolving-agent/internal/atebridge"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateports"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

type stateOnly struct{ StateStore }

type cutHistory struct {
	HistoryReader
	ateports.HistoryCutStore
	EvidenceSourceProvider
}

type nativeEvidenceHistory struct {
	ateports.HistoryCutStore
	HistoryReader
	resolve func(context.Context, ateports.Record) ([]memory.Source, error)
}

func (h *nativeEvidenceHistory) EvidenceSources(ctx context.Context, record ateports.Record) ([]memory.Source, error) {
	return h.resolve(ctx, record)
}

func TestNewRequiresExplicitHistoryAndState(t *testing.T) {
	f := newFixture(t)
	var absentHistory *historyStore
	var absentState *stateOnly
	for _, options := range []Options{
		{Bridge: f.b, State: f.state},
		{Bridge: f.b, State: f.state, History: absentHistory},
		{Bridge: f.b, History: f.history},
		{Bridge: f.b, State: absentState, History: f.history},
	} {
		if _, err := New(options); err == nil {
			t.Fatal("accepted missing dependency or inferred history from state")
		}
	}
}

func TestIndependentHistoryAuthorityOwnsCutsAndCitationRecovery(t *testing.T) {
	f := newFixture(t)
	f.events(t, "independent", 1)
	source := &historyStore{count: 3}
	through := uint64(3)
	bounded := &boundedHistoryStore{
		growingHistoryStore: &growingHistoryStore{StateStore: f.state, nativeHistory: f.history},
		t:                   t, key: "independent-history", boundary: &through,
		readPage: func(ctx context.Context, id string, after, through uint64, limit int) ([]ateports.Record, error) {
			return source.History(ctx, id, after, limit)
		},
	}
	options := Options{
		Bridge: f.b, State: stateOnly{f.state},
		History: cutHistory{HistoryReader: source, HistoryCutStore: bounded, EvidenceSourceProvider: f.history},
	}
	s, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.originalHistory(t.Context(), bounded.key, "independent")
	if err != nil || !h.Complete || h.Coverage.Cut != 3 || len(h.Events) != 3 || bounded.cuts != 1 {
		t.Fatalf("wrong source authority: %+v %v", h.Coverage, err)
	}
	// Recreate the service and recover citations through History, not the KV.
	s, err = New(options)
	if err != nil {
		t.Fatal(err)
	}
	h, err = s.originalHistory(t.Context(), bounded.key, "independent")
	if err != nil || bounded.cuts != 1 {
		t.Fatalf("replay changed the cut: %v", err)
	}
	records, err := s.recordsFor(t.Context(), h, map[string]bool{"1": true, "3": true})
	if err != nil || len(records) != 2 {
		t.Fatalf("citation recovery: %v", err)
	}
	recovered, err := s.contextEvidence(t.Context(), Context{
		SessionID: "independent", EvidenceReferences: evidenceReferences(records),
	})
	if err != nil || !reflect.DeepEqual(records, recovered) {
		t.Fatalf("source envelope changed: %v", err)
	}

}

func TestNativeEvidenceResolutionDoesNotProjectOrFallBack(t *testing.T) {
	f := newFixture(t)
	f.events(t, "native-reference", 2)
	records, err := f.history.History(t.Context(), "native-reference", 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	// Seed existing anchors once. The provider resolves them without invoking
	// the bridge, and can return multiple dependencies for one original event.
	existing, err := f.s.anchorRecords(t.Context(), records)
	if err != nil {
		t.Fatal(err)
	}
	provider := &nativeEvidenceHistory{HistoryReader: f.history, HistoryCutStore: f.history}
	s, err := New(Options{Bridge: f.b, State: stateOnly{f.state}, History: provider})
	if err != nil {
		t.Fatal(err)
	}
	unavailable := errors.New("native source unavailable")
	for _, mode := range []string{"existing", "missing", "invalid", "error", "partial"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			provider.resolve = func(_ context.Context, record ateports.Record) ([]memory.Source, error) {
				if !reflect.DeepEqual(record, records[calls]) {
					t.Fatal("provider did not receive the exact source envelope")
				}
				calls++
				switch mode {
				case "missing":
					return nil, nil
				case "invalid":
					return []memory.Source{{}}, nil
				case "error":
					return nil, unavailable
				case "partial":
					if calls == 2 {
						return nil, unavailable
					}
				}
				return existing[calls-1 : calls], nil
			}
			commits := f.p.commits
			got, err := s.anchorRecords(t.Context(), records)
			if mode == "existing" {
				want := existing
				if err != nil || !reflect.DeepEqual(got, want) || calls != 2 {
					t.Fatalf("native dependencies changed: %v", err)
				}
			} else if err == nil || got != nil {
				t.Fatalf("provider failure exposed partial results: %v", err)
			} else if (mode == "error" || mode == "partial") && !errors.Is(err, unavailable) {
				t.Fatalf("provider error lost: %v", err)
			}
			if f.p.commits != commits {
				t.Fatal("native provider used bridge projection")
			}
		})
	}
}

func TestNativeEvidenceChecksRetractionInSourceSpace(t *testing.T) {
	f := newFixture(t)
	f.events(t, "native-space", 1)
	records, err := f.history.History(t.Context(), "native-space", 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	sources, err := f.s.anchorRecords(t.Context(), records)
	if err != nil {
		t.Fatal(err)
	}
	if sources[0].Anchor.Space == f.b.OperationalSpace() {
		t.Fatal("source projected into execution space")
	}
	s := f.s
	value := Context{SessionID: "native-space", Sources: sources, EvidenceReferences: evidenceReferences(records)}
	commits := f.p.commits
	if err = s.checkSources(t.Context(), value); err != nil || f.p.commits != commits {
		t.Fatalf("native source check wrote projections or failed: %v", err)
	}
	if err = s.applyMemory(t.Context(), "native-retraction", sources[0].Anchor.Space, memory.Command{
		RetractSource: &memory.RetractSourceCommand{
			Source: sources[0].SourceID, Retraction: memory.Retraction{Kind: memory.RetractionUseBan, Reason: "native source withdrawn"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err = s.checkSources(t.Context(), value); err == nil {
		t.Fatal("native source space retraction bypassed")
	}
}

func TestModelJSONClosedBoundedAndRequired(t *testing.T) {
	for _, raw := range []string{
		`{"passed":true,"reason":"x","event_ids":["e"],"command":"delete"}`,
		`{"passed":true,"passed":false,"reason":"x","event_ids":["e"]}`,
		`{"passed":true,"event_ids":["e"]}`,
		`{"passed":null,"reason":"x","event_ids":["e"]}`,
		`{"passed":true,"reason":"x","event_ids":null}`,
		`{"passed":true,"reason":"x","event_ids":["e"]} {}`,
		`{"passed":"true","reason":"x","event_ids":["e"]}`,
		strings.Repeat(" ", maxModelBytes) + `{}`,
	} {
		var out review
		if !errors.Is(strictJSON([]byte(raw), &out), ErrDecision) {
			t.Fatalf("accepted invalid output: %.100s", raw)
		}
	}
	var valid review
	if err := strictJSON([]byte(`{"passed":false,"reason":"not supported","event_ids":[],"claims_tool_success":false,"tool_success_event_ids":[]}`), &valid); err != nil {
		t.Fatal(err)
	}
	if err := citations([]string{"invented"}, map[string]bool{"actual": true}, true); !errors.Is(err, ErrDecision) {
		t.Fatal("unknown citation accepted")
	}
}

type failedLeaseProcess struct{ base *testProcess }

func (p failedLeaseProcess) WithClient(ctx context.Context, fn func(*client.Client) error) error {
	if err := p.base.WithClient(ctx, fn); err != nil {
		return err
	}
	return context.DeadlineExceeded
}

func TestFailedReadLeaseReturnsNoPartialCatalog(t *testing.T) {
	f := newFixture(t)
	r := route{NewSpace: &newSpace{Name: "Known", Description: "Must not escape a failed read"}}
	if err := f.s.applyRoute(t.Context(), "read-lease", r); err != nil {
		t.Fatal(err)
	}
	b, err := atebridge.New(atebridge.Options{Process: failedLeaseProcess{base: f.p}, Pending: f.db.ATERequests(), State: f.state, Namespace: digest(f.path)})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Bridge: b, State: f.state, History: f.history})
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.directory(t.Context())
	if !errors.Is(err, context.DeadlineExceeded) || len(d.Spaces) != 0 {
		t.Fatal("failed directory read exposed results")
	}
	c, err := s.retrieve(t.Context(), routeSpaces("read-lease", r), "", nil)
	if !errors.Is(err, context.DeadlineExceeded) || len(c.Spaces) != 0 || len(c.Memories) != 0 {
		t.Fatal("failed retrieval exposed results")
	}
}

type historyStore struct {
	count int
	calls int
}

func (s *historyStore) History(ctx context.Context, id string, after uint64, limit int) ([]ateports.Record, error) {
	s.calls++
	out := []ateports.Record{}
	for i := after + 1; i <= uint64(s.count) && len(out) < limit; i++ {
		raw, _ := json.Marshal(session.Event{SessionID: session.ID(id), Sequence: i, EventID: fmt.Sprint(i), Type: session.EventUserMessage, Payload: json.RawMessage(`{"content":"original"}`)})
		out = append(out, ateports.Record{Stream: ateports.StreamSession, SessionID: id, SourceID: fmt.Sprint(i), Sequence: i, Kind: string(session.EventUserMessage), Payload: raw})
	}
	return out, nil
}

func TestHistoryChunksPreserveEveryOriginalByte(t *testing.T) {
	raw, _ := json.Marshal(map[string]string{"text": strings.Repeat("原始完整 evidence ", 10000)})
	h := history{Complete: true, Events: []evidenceEvent{{SourceID: "actual-event", Sequence: 1, Payload: raw}}}
	chunks := historyChunks(h)
	if len(chunks) < 2 {
		t.Fatal("large event not chunked")
	}
	var reconstructed strings.Builder
	for i, chunk := range chunks {
		if len(chunk) != 1 || chunk[0].Part != i+1 || chunk[0].Parts != len(chunks) {
			t.Fatal("fragment order lost")
		}
		reconstructed.WriteString(chunk[0].OriginalJSON)
	}
	if reconstructed.String() != string(raw) {
		t.Fatal("original event bytes truncated or changed")
	}
}

func TestMapReduceCoversAllOriginalChunksAndReplays(t *testing.T) {
	f := newFixture(t)
	h := history{Complete: true}
	for i := 0; i < 36; i++ {
		raw, _ := json.Marshal(strings.Repeat("evidence ", 3500))
		h.Events = append(h.Events, evidenceEvent{SourceID: fmt.Sprintf("event-%d", i), Sequence: uint64(i + 1), Payload: raw})
	}
	seen := map[string]bool{}
	reductions := 0
	generate := func(_ context.Context, messages []*schema.Message) (*schema.Message, error) {
		result := notes{Notes: []note{}}
		if strings.Contains(messages[0].Content, "Stage: history-map") {
			var fs []fragment
			if err := json.Unmarshal([]byte(messages[1].Content), &fs); err != nil {
				t.Fatal(err)
			}
			for _, v := range fs {
				seen[v.EventID] = true
				result.Notes = append(result.Notes, note{Text: "observed", EventIDs: []string{v.EventID}})
			}
		} else {
			reductions++
			var input []note
			if err := json.Unmarshal([]byte(messages[1].Content), &input); err != nil {
				t.Fatal(err)
			}
			ids := []string{}
			for id := range noteIDs(input) {
				ids = append(ids, id)
			}
			result.Notes = append(result.Notes, note{Text: "bounded reduction", EventIDs: ids})
		}
		raw, _ := json.Marshal(result)
		return schema.AssistantMessage(string(raw), nil), nil
	}
	result, err := f.s.summarize(t.Context(), "map-reduce", h, generate)
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 36 || reductions == 0 || len(result.Notes) > 12 {
		t.Fatalf("coverage=%d reduce=%d result=%d", len(seen), reductions, len(result.Notes))
	}
	if _, err = f.s.summarize(t.Context(), "map-reduce", h, nil); err != nil {
		t.Fatal(err)
	}
}

func TestTrivialConversationCreatesNoLesson(t *testing.T) {
	f := newFixture(t)
	m := newModel(t, f)
	if _, err := f.history.Append(t.Context(), "hello", 0, session.PendingEvent{Type: session.EventUserMessage, Payload: json.RawMessage(`{"text":"hello"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Complete(t.Context(), "hello", "turn", m.generate); err != nil {
		t.Fatal(err)
	}
	if m.calls["verify"] != 0 || m.calls["discover"] != 0 {
		t.Fatal("trivial conversation created a lesson")
	}
}

func TestLargeNativeEvidenceWithdrawalBlocksDerivedUse(t *testing.T) {
	f := newFixture(t)
	raw, _ := json.Marshal(map[string]string{"text": strings.Repeat("large original ", 80000)})
	if _, err := f.history.Append(t.Context(), "large", 0, session.PendingEvent{Type: session.EventToolSucceeded, Payload: raw}); err != nil {
		t.Fatal(err)
	}
	records, err := f.history.History(t.Context(), "large", 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(records[0].Payload) <= api.MaxStateValueBytes {
		t.Fatal("fixture not segmented")
	}
	sources, err := f.s.anchorRecords(t.Context(), records)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 {
		t.Fatal("expected original native session node")
	}
	value := Context{SessionID: "large", Sources: sources, EvidenceReferences: evidenceReferences(records)}
	if err = f.s.checkSources(t.Context(), value); err != nil {
		t.Fatal(err)
	}
	segment := sources[0]
	if err = f.s.applyMemory(t.Context(), "segment-withdrawal", segment.Anchor.Space, memory.Command{RetractSource: &memory.RetractSourceCommand{
		Source: segment.SourceID, Retraction: memory.Retraction{Kind: memory.RetractionUseBan, Reason: "segment withdrawn"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err = f.s.checkSources(t.Context(), value); err == nil {
		t.Fatal("segment withdrawal bypassed by manifest")
	}
}

package experience

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"unicode/utf8"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateports"
)

type (
	history struct {
		// Native snapshots retain only coverage and a checksum. Source bytes
		// and model projections are reconstructed from the frozen History cut.
		Events         []evidenceEvent `json:",omitempty"`
		SessionID      string
		Coverage       HistoryCoverage
		Complete       bool
		Reason         string
		Through        uint64
		ReferencesOnly bool   `json:",omitempty"`
		EventsSHA256   string `json:",omitempty"`
	}
	fragment struct {
		EventID      string `json:"event_id"`
		Kind         string `json:"kind"`
		Sequence     uint64 `json:"sequence"`
		Part         int    `json:"part"`
		Parts        int    `json:"parts"`
		OriginalJSON string `json:"original_json"`
	}
)

const notesSchema = `{"notes":[{"text":"string, at most 1024 bytes","event_ids":["supplied original event ID"]}]}`

// originalHistory pins the source high-watermark before reading bounded pages.
func (s *Service) originalHistory(ctx context.Context, key, session string) (history, error) {
	var h history
	var expected *history
	if found, err := s.load(ctx, key, &h); err != nil {
		return h, err
	} else if found {
		if h.SessionID != session {
			return h, ErrSelection
		}
		if h.Coverage.Policy != primaryPolicy {
			return h, fmt.Errorf("%w: stored history predates frozen primary-evidence coverage; use a new turn rather than reselecting this replay", ErrIncomplete)
		}
		if !h.Complete {
			return h, fmt.Errorf("%w: %s", ErrIncomplete, h.Reason)
		}
		if !h.ReferencesOnly || h.EventsSHA256 == "" || len(h.Events) != 0 {
			return history{}, fmt.Errorf("%w: native history checkpoint must contain references only", ErrIncomplete)
		}
		saved := h
		expected = &saved
		h = history{}
	}
	var cut historyCut
	haveCut, err := s.load(ctx, key+"/cut", &cut)
	if err != nil {
		return h, err
	}
	if expected != nil && (!haveCut || cut.Through != expected.Coverage.Cut || cut.Binding != expected.Coverage.Binding) {
		return h, ErrSelection
	}
	cutStore := s.history.(ateports.HistoryCutStore)
	if !haveCut {
		cut.Through, err = cutStore.HistoryCut(ctx, session)
		if err != nil {
			return h, err
		}
		cut.Binding = "persisted_history_cut"
		if err = s.save(ctx, key+"/cut", cut); err != nil {
			return h, err
		}
	}
	h.SessionID = session
	h.Coverage = HistoryCoverage{Policy: primaryPolicy, Cut: cut.Through, Binding: cut.Binding, ExcludedNonprimary: map[string]int{}}
	seen := map[string]bool{}
	for h.Through < cut.Through {
		page, err := cutStore.HistoryThrough(ctx, session, h.Through, cut.Through, maxPageItems)
		if err != nil {
			return h, err
		}
		if len(page) > maxPageItems {
			return h, fmt.Errorf("%w: history page exceeds requested limit", ErrIncomplete)
		}
		if len(page) == 0 {
			break
		}
		for _, r := range page {
			if r.Stream != ateports.StreamSession || r.SessionID != session || r.Sequence <= h.Through || r.Sequence > cut.Through || r.SourceID == "" || seen[r.SourceID] || !json.Valid(r.Payload) {
				return h, fmt.Errorf("%w: invalid original history order or identity", ErrDecision)
			}
			h.Through = r.Sequence
			seen[r.SourceID] = true
			h.Coverage.SourceEvents++
			event, excluded, err := projectEvidence(r)
			if err != nil {
				return h, err
			}
			if excluded != "" {
				h.Coverage.ExcludedNonprimary[excluded]++
				continue
			}
			h.Coverage.PrimaryEvents++
			h.Coverage.PrimaryBytes += len(event.Payload)
			if event.Projected {
				h.Coverage.ProjectedWrappers++
			}
			if h.Coverage.PrimaryEvents > MaxHistoryEvents || h.Coverage.PrimaryBytes > MaxHistoryBytes {
				h.Reason = fmt.Sprintf("primary evidence exceeds %d events or %d bytes; original source journal remains intact", MaxHistoryEvents, MaxHistoryBytes)
				break
			}
			// Recover exact originals by checksum-bound reference when cited.
			// Do not accumulate large receipt wrappers across pages.
			h.Events = append(h.Events, event)
		}
		if h.Reason != "" {
			break
		}
	}
	if h.Through != cut.Through && h.Reason == "" {
		h.Reason = "frozen history cut unavailable"
	}
	h.Complete = h.Reason == ""
	h.Coverage.Complete = h.Complete
	h.ReferencesOnly = true
	h.EventsSHA256 = digest(h.Events)
	snapshot := h
	snapshot.Events = nil
	if expected != nil {
		if !reflect.DeepEqual(snapshot, *expected) {
			return history{}, fmt.Errorf("%w: frozen native history changed", ErrSelection)
		}
	} else if err = s.save(ctx, key, snapshot); err != nil {
		return h, err
	}
	if !h.Complete {
		return h, fmt.Errorf("%w: %s", ErrIncomplete, h.Reason)
	}
	return h, nil
}

func splitUTF8(text string, size int) []string {
	var parts []string
	for len(text) > size {
		n := size
		for n > 0 && !utf8.RuneStart(text[n]) {
			n--
		}
		parts = append(parts, text[:n])
		text = text[n:]
	}
	return append(parts, text)
}

func historyChunks(h history) [][]fragment {
	var chunks [][]fragment
	var chunk []fragment
	size := 0
	for _, r := range h.Events {
		parts := splitUTF8(string(r.Payload), 24<<10)
		for i, part := range parts {
			if size+len(part) > 24<<10 && len(chunk) > 0 {
				chunks = append(chunks, chunk)
				chunk = nil
				size = 0
			}
			chunk = append(chunk, fragment{EventID: r.SourceID, Kind: r.Kind, Sequence: r.Sequence, Part: i + 1, Parts: len(parts), OriginalJSON: part})
			size += len(part)
		}
	}
	if len(chunk) > 0 {
		chunks = append(chunks, chunk)
	}
	return chunks
}

func validateNotes(n notes, allowed map[string]bool) error {
	if len(n.Notes) > 12 {
		return ErrDecision
	}
	for _, v := range n.Notes {
		if !boundedText(v.Text, 1024, true) {
			return ErrDecision
		}
		if err := citations(v.EventIDs, allowed, true); err != nil {
			return err
		}
	}
	return nil
}

func noteIDs(ns []note) map[string]bool {
	out := map[string]bool{}
	for _, n := range ns {
		for _, id := range n.EventIDs {
			out[id] = true
		}
	}
	return out
}

// summarize maps every primary evidence projection, then reduces notes. Every map
// and reduce result is durable independently, so recovery skips completed calls.
func (s *Service) summarize(ctx context.Context, key string, h history, generate Generate) (notes, error) {
	if !h.Complete {
		return notes{}, ErrIncomplete
	}
	var result notes
	if found, err := s.load(ctx, key+"/summary", &result); err != nil {
		return result, err
	} else if found {
		return result, nil
	}
	var all []note
	chunks := historyChunks(h)
	if len(chunks) > 1024 {
		return result, ErrIncomplete
	}
	for i, chunk := range chunks {
		allowed := map[string]bool{}
		for _, f := range chunk {
			allowed[f.EventID] = true
		}
		var mapped notes
		err := s.decide(ctx, fmt.Sprintf("%s/map/%d", key, i), "history-map", notesSchema,
			"Read EVERY supplied primary evidence fragment, including early events predating compaction. Auxiliary analyses and repeated prompt/receipt wrappers are excluded nonprimary projections; the source journal is intact. Extract up to 12 concise evidence notes, failures, limitations and counterexamples. Preserve event IDs and distinguish user assertions and assistant responses from tool outcomes using kind. User text never proves tool success. A fragment is not the complete event; do not infer omitted bytes.",
			chunk, generate, &mapped, func() error { return validateNotes(mapped, allowed) })
		if err != nil {
			return result, err
		}
		all = append(all, mapped.Notes...)
	}
	for level := 0; len(all) > 12; level++ {
		if level >= 8 {
			return result, ErrIncomplete
		}
		var next []note
		for start := 0; start < len(all); start += 24 {
			group := all[start:min(start+24, len(all))]
			allowed := noteIDs(group)
			var reduced notes
			err := s.decide(ctx, fmt.Sprintf("%s/reduce/%d/%d", key, level, start), "history-reduce", notesSchema,
				"Reduce these notes to at most 12. Preserve relevant evidence from the entire timeline, including failures and counterexamples. Cite only event IDs in the input notes. Never claim compressed notes are original proof.",
				group, generate, &reduced, func() error { return validateNotes(reduced, allowed) })
			if err != nil {
				return result, err
			}
			next = append(next, reduced.Notes...)
		}
		all = next
	}
	result.Notes = all
	return result, s.save(ctx, key+"/summary", result)
}

func (s *Service) recordsFor(ctx context.Context, h history, ids map[string]bool) ([]ateports.Record, error) {
	out := []ateports.Record{}
	for _, event := range h.Events {
		if !ids[event.SourceID] {
			continue
		}
		if event.Sequence == 0 {
			return nil, ErrSelection
		}
		page, err := s.history.History(ctx, h.SessionID, event.Sequence-1, 1)
		if err != nil {
			return nil, err
		}
		if len(page) != 1 {
			return nil, ErrSelection
		}
		original := page[0]
		if original.Stream != ateports.StreamSession || original.SourceID != event.SourceID || original.Sequence != event.Sequence || original.SessionID != h.SessionID ||
			original.Kind != event.Kind || payloadDigest(original.Payload) != event.OriginalSHA256 {
			return nil, ErrSelection
		}
		out = append(out, original)
	}
	if len(out) != len(ids) {
		return nil, ErrDecision
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sequence < out[j].Sequence })
	return out, nil
}

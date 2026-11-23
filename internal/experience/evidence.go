package experience

import (
	"context"
	"errors"
	"reflect"

	"github.com/vkviyu/agent-temporal-engine/memory"
	"github.com/vkviyu/agent-temporal-engine/spacecatalog"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateports"
)

// EvidenceSourceProvider is a required capability of Options.History. It
// resolves an exact original Record to existing native anchors without writing
// projections or acknowledging delivery. Implementations must verify the full
// source identity and payload, enforce current disclosure/reuse policy, and
// return a stable, nonempty list of object identities and segment dependencies.
// Read fences may advance; experience pins the first verified anchors. Missing,
// changed, or inaccessible sources must return an error, never partial results.
// The provider owns the native representation; callers do not assume that its
// anchors contain bridge byte values or bridge segment manifests.
type EvidenceSourceProvider interface {
	EvidenceSources(ctx context.Context, record ateports.Record) ([]memory.Source, error)
}

// anchorRecords resolves only original native sources. Provider failures never
// fall back to copying source payloads into bridge-managed values.
func (s *Service) anchorRecords(ctx context.Context, records []ateports.Record) ([]memory.Source, error) {
	provider := s.history.(EvidenceSourceProvider)
	sources := []memory.Source{}
	for _, record := range records {
		resolved, err := provider.EvidenceSources(ctx, record)
		if err != nil {
			return nil, err
		}
		if len(resolved) == 0 {
			return nil, ErrSelection
		}
		for _, source := range resolved {
			if !spacecatalog.ValidSource(source) {
				return nil, ErrSelection
			}
		}
		resolved, err = s.pinNativeSources(ctx, record, resolved)
		if err != nil {
			return nil, err
		}
		sources = append(sources, resolved...)
	}
	return sources, nil
}

type nativeSourcePin struct {
	Reference EvidenceReference
	Sources   []memory.Source
}

// pinNativeSources stores only checksummed references, never event bytes.
// Fresh resolution enforces current source policy before returning the original
// provenance anchors. All State operations occur outside a bridge lease.
func (s *Service) pinNativeSources(ctx context.Context, record ateports.Record, sources []memory.Source) ([]memory.Source, error) {
	key := "experience/native-source/v1/" + digest([]string{
		string(s.bridge.OperationalSpace()), record.Stream, record.SessionID, record.SourceID,
	})
	reference := evidenceReferences([]ateports.Record{record})[0]
	var pin nativeSourcePin
	found, err := s.load(ctx, key, &pin)
	if err != nil {
		return nil, err
	}
	if !found {
		err := s.save(ctx, key, nativeSourcePin{Reference: reference, Sources: sources})
		if err != nil && !errors.Is(err, ateports.ErrConflict) {
			return nil, err
		}
		if found, err = s.load(ctx, key, &pin); err != nil {
			return nil, err
		} else if !found {
			return nil, ErrSelection
		}
	}
	if pin.Reference != reference || len(pin.Sources) != len(sources) {
		return nil, ErrSelection
	}
	for i, fresh := range sources {
		old := pin.Sources[i]
		if !spacecatalog.ValidSource(old) || old.SourceID != fresh.SourceID ||
			old.Anchor.Fence.Store != fresh.Anchor.Fence.Store ||
			old.Anchor.Fence.LSN > fresh.Anchor.Fence.LSN ||
			(old.Anchor.Fence.LSN == fresh.Anchor.Fence.LSN && old.Anchor.Fence != fresh.Anchor.Fence) {
			return nil, ErrSelection
		}
		old.Anchor.Fence = fresh.Anchor.Fence
		if !reflect.DeepEqual(old.Anchor, fresh.Anchor) {
			return nil, ErrSelection
		}
	}
	return pin.Sources, nil
}

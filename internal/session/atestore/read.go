package atestore

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/vkviyu/agent-temporal-engine/agentprofile"
	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateports"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

// NativeReader is the typed ATE read boundary. No full agent snapshot, local
// projection, or database is needed to serve a session page.
type NativeReader interface {
	payloadReader
	OpenView(context.Context, api.OpenViewRequest) (api.ViewDescriptor, error)
	CloseView(context.Context, api.CloseViewRequest) error
	GetSession(context.Context, api.ReadSessionRequest) (api.SessionResult, error)
	ListSessionEvents(context.Context, api.ListSessionEventsRequest) (api.SessionEventsPage, error)
}

var _ NativeReader = (*client.Client)(nil)

func (s *Store) getSession(ctx context.Context, reader NativeReader, id session.ID) (api.SessionResult, error) {
	return s.getSessionAt(ctx, reader, id, nil)
}

func (s *Store) getSessionAt(ctx context.Context, reader NativeReader, id session.ID, anchor *model.SnapshotAnchor) (api.SessionResult, error) {
	out, err := reader.GetSession(ctx, api.ReadSessionRequest{Space: s.space, Principal: "gsea", SessionID: string(id), Anchor: anchor})
	if err != nil {
		return out, err
	}
	if out.Anchor.Space != s.space || out.Anchor.Fence.Store != s.storeID || out.Anchor.Validate() != nil ||
		out.Anchor.Cursor != nil || out.Anchor.Knowledge != nil || (anchor != nil && out.Anchor != *anchor) {
		return api.SessionResult{}, ErrIdentity
	}
	if out.Found && (out.Session.ID != string(id) || out.Session.Space != s.space || out.Session.Owner != "gsea") {
		return api.SessionResult{}, ErrCorrupt
	}
	return out, nil
}

func (s *Store) nativeSession(ctx context.Context, reader NativeReader, id session.ID) (agentprofile.Session, bool, error) {
	out, err := s.getSession(ctx, reader, id)
	return out.Session, out.Found, err
}

func (s *Store) CurrentSequence(ctx context.Context, id session.ID) (uint64, error) {
	if err := checkID(id); err != nil {
		return 0, err
	}
	results := make(chan uint64, 1)
	err := s.withClient(ctx, func(ctx context.Context, c *client.Client) error {
		out, err := s.getSession(ctx, c, id)
		if err == nil {
			results <- out.Session.Sequence
		}
		return err
	})
	if err != nil {
		return 0, err
	}
	select {
	case sequence := <-results:
		return sequence, nil
	default:
		return 0, errors.New("atestore: lease did not complete read")
	}
}

func (s *Store) Load(ctx context.Context, id session.ID, after uint64, limit int) ([]session.Event, error) {
	items, err := s.read(ctx, id, after, nil, nil, limit)
	if err != nil {
		return nil, err
	}
	return decodeItems(items)
}

// LoadByCorrelation returns the entire matching operation, or ErrLimit. It
// never returns a truncated operation as though it were complete. The native
// API treats an empty correlation filter as "all"; empty-ID lookups therefore
// use a bounded native scan and retain only exact empty-ID matches.
func (s *Store) LoadByCorrelation(ctx context.Context, id session.ID, correlation string) ([]session.Event, error) {
	if correlation != "" && !validIdentity(correlation) {
		return nil, session.ErrInvalidEvent
	}
	items, err := s.read(ctx, id, 0, nil, &correlation, ateports.MaxLimit)
	if err != nil {
		return nil, err
	}
	return decodeItems(items)
}

func decodeItems(items []resolvedItem) ([]session.Event, error) {
	events := make([]session.Event, len(items))
	for i, item := range items {
		events[i] = item.decoded
	}
	return events, nil
}

func (s *Store) read(ctx context.Context, id session.ID, after uint64, through *uint64, correlation *string, limit int) ([]resolvedItem, error) {
	if err := checkID(id); err != nil {
		return nil, err
	}
	if ctx == nil {
		return nil, errors.New("atestore: context required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if through != nil && after >= *through {
		if after > *through {
			return nil, fmt.Errorf("%w: after exceeds cut", ateports.ErrInvalid)
		}
		if *through == 0 {
			return []resolvedItem{}, nil
		}
	}
	results := make(chan []resolvedItem, 1)
	err := s.withClient(ctx, func(ctx context.Context, c *client.Client) error {
		items, err := s.readNative(ctx, c, id, after, through, correlation, pageLimit(limit))
		if err == nil {
			results <- items
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	select {
	case items := <-results:
		return items, nil
	default:
		return nil, errors.New("atestore: lease did not complete read")
	}
}

func (s *Store) readNative(ctx context.Context, reader NativeReader, id session.ID, after uint64, through *uint64, correlation *string, limit int) (out []resolvedItem, err error) {
	view, err := reader.OpenView(ctx, api.OpenViewRequest{Space: s.space, Principal: "gsea", Mode: "state"})
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, closeView(ctx, reader, view.Ref))
		if err != nil {
			out = nil
		}
	}()
	if view.Anchor.Validate() != nil || view.Anchor.Space != s.space ||
		view.Anchor.Fence.Store != s.storeID || view.Anchor.Cursor != nil || view.Anchor.Knowledge != nil {
		return nil, ErrIdentity
	}
	// Keep the anchor pinned through every event page and payload read.
	state, err := s.getSessionAt(ctx, reader, id, &view.Anchor)
	if err != nil {
		return nil, err
	}
	end := state.Session.Sequence
	if through != nil {
		if *through > end {
			return nil, fmt.Errorf("%w: cut exceeds native history", ErrCorrupt)
		}
		end = *through
	}
	items := make([]resolvedItem, 0)
	if !state.Found || after >= end {
		return items, nil
	}
	if correlation == nil && uint64(limit) < end-after {
		end = after + uint64(limit)
	}
	request := api.ListSessionEventsRequest{Space: s.space, Principal: "gsea",
		Anchor: &state.Anchor, SessionID: string(id), AfterSequence: after,
		ThroughSequence: end, Limit: min(limit, api.MaxSessionPageItems)}
	if correlation != nil {
		request.CorrelationID = *correlation
	}
	totalBytes, scanned := 0, 0
	last := after
	total := -1
	// Every nonterminal page must advance by at least one event. The count
	// bound therefore also bounds RPCs, even for unusually large envelopes.
	for pages := 0; pages < ateports.MaxLimit; pages++ {
		page, err := reader.ListSessionEvents(ctx, request)
		if err != nil {
			return nil, err
		}
		if !page.Found || page.ThroughSequence != end || page.Anchor != state.Anchor ||
			(total >= 0 && page.Total != total) {
			return nil, ErrCorrupt
		}
		total = page.Total
		if total < 0 || total > limit {
			return nil, ErrLimit
		}
		if len(page.Items) > request.Limit || (len(page.Items) == 0 && (page.Continuation != "" || last < end && request.CorrelationID == "")) {
			return nil, ErrCorrupt
		}
		for _, item := range page.Items {
			event := item.Event
			ref, err := event.Ref()
			if err != nil || item.Ref != ref || item.Anchor != state.Anchor ||
				event.Space != s.space || event.Owner != "gsea" || event.Session != string(id) ||
				event.Sequence <= last || event.Sequence > end ||
				(request.CorrelationID == "" && (last == math.MaxUint64 || event.Sequence != last+1)) {
				return nil, ErrCorrupt
			}
			if request.CorrelationID != "" && event.Event.CorrelationID != request.CorrelationID {
				return nil, ErrCorrupt
			}
			resolved, err := resolveItem(ctx, reader, item, MaxReadBytes-totalBytes)
			if err != nil {
				return nil, err
			}
			last = event.Sequence
			scanned++
			totalBytes += len(resolved.envelope)
			if scanned > limit || totalBytes > MaxReadBytes {
				return nil, ErrLimit
			}
			if correlation == nil || event.Event.CorrelationID == *correlation {
				items = append(items, resolved)
			}
		}
		if page.Continuation == "" {
			if scanned != total || (request.CorrelationID == "" && last != end) {
				return nil, ErrCorrupt
			}
			return items, nil
		}
		if page.Continuation == request.Continuation || scanned >= limit {
			return nil, ErrCorrupt
		}
		request.Continuation = page.Continuation
	}
	return nil, ErrLimit
}

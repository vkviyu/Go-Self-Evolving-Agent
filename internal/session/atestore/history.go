package atestore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/vkviyu/agent-temporal-engine/agentprofile"
	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/memory"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateports"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

func (s *Store) History(ctx context.Context, id string, after uint64, limit int) ([]ateports.Record, error) {
	items, err := s.read(ctx, session.ID(id), after, nil, nil, limit)
	if err != nil {
		return nil, err
	}
	return historyRecords(items)
}

func (s *Store) HistoryCut(ctx context.Context, id string) (uint64, error) {
	return s.CurrentSequence(ctx, session.ID(id))
}

func (s *Store) HistoryThrough(ctx context.Context, id string, after, through uint64, limit int) ([]ateports.Record, error) {
	items, err := s.read(ctx, session.ID(id), after, &through, nil, limit)
	if err != nil {
		return nil, err
	}
	return historyRecords(items)
}

func historyRecords(items []resolvedItem) ([]ateports.Record, error) {
	records := make([]ateports.Record, len(items))
	for i, item := range items {
		event := item.decoded
		// ID intentionally stays zero: native history has no delivery queue row.
		records[i] = ateports.Record{Stream: ateports.StreamSession,
			SourceID: event.EventID, SessionID: string(event.SessionID), Sequence: event.Sequence,
			Kind: string(event.Type), OccurredAt: event.OccurredAt,
			Payload: bytes.Clone(item.envelope)}
	}
	return records, nil
}

// EvidenceSources implements experience's native evidence provider port. It
// resolves a previously committed SessionEvent node at a pinned read anchor;
// it never creates an evidence value, invokes a bridge, or submits a command.
func (s *Store) EvidenceSources(ctx context.Context, record ateports.Record) ([]memory.Source, error) {
	if record.Stream != ateports.StreamSession || record.SourceID == "" ||
		record.Sequence == 0 || len(record.Payload) > MaxReadBytes {
		return nil, ateports.ErrInvalid
	}
	record.Payload = bytes.Clone(record.Payload)
	var event session.Event
	if err := json.Unmarshal(record.Payload, &event); err != nil {
		return nil, ErrCorrupt
	}
	// RawMessage decoding removes framing whitespace. Verify the complete
	// original envelope against decoded native history below, including checksum.
	if event.SessionID != session.ID(record.SessionID) || event.EventID != record.SourceID ||
		event.Sequence != record.Sequence || string(event.Type) != record.Kind ||
		!event.OccurredAt.Equal(record.OccurredAt) {
		return nil, ErrCorrupt
	}
	results := make(chan memory.Source, 1)
	err := s.withClient(ctx, func(ctx context.Context, c *client.Client) error {
		items, err := s.readNative(ctx, c, event.SessionID, event.Sequence-1, &event.Sequence, nil, 1)
		if err != nil {
			return err
		}
		if len(items) != 1 || !bytes.Equal(items[0].envelope, record.Payload) {
			return ErrCorrupt
		}
		item := items[0]
		view, err := c.OpenView(ctx, api.OpenViewRequest{Space: s.space, Principal: "gsea", Anchor: &item.Anchor})
		if err != nil {
			return err
		}
		node, readErr := c.GetNode(ctx, api.GetNodeRequest{View: view.Ref, Ref: item.Ref})
		closeErr := closeView(ctx, c, view.Ref)
		if err := errors.Join(readErr, closeErr); err != nil {
			return err
		}
		if node.Ref != item.Ref || node.Payload != item.Payload || node.Anchor != item.Anchor ||
			!model.ValidDigest(string(node.Payload)) {
			return ErrCorrupt
		}
		var native agentprofile.SessionEvent
		if err := json.Unmarshal(node.Data, &native); err != nil {
			return ErrCorrupt
		}
		ref, err := native.Ref()
		if err != nil || ref != item.Ref {
			return ErrCorrupt
		}
		results <- memory.Source{SourceID: node.Payload, Anchor: node.Anchor}
		return nil
	})
	if err != nil {
		return nil, err
	}
	select {
	case source := <-results:
		return []memory.Source{source}, nil
	default:
		return nil, errors.New("atestore: lease did not complete evidence read")
	}
}

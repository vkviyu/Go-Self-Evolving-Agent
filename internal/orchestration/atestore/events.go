package atestore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

// sha256Hex returns the hexadecimal SHA-256 fingerprint of the supplied bytes.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Keep the canonical encoding identical to the SQL event store.
func orchestrationChecksum(event orchestration.Event) (string, error) {
	canonical := struct {
		EventID        orchestration.EventID    `json:"event_id"`
		GlobalSequence uint64                   `json:"global_sequence"`
		StreamType     orchestration.StreamType `json:"stream_type"`
		StreamID       string                   `json:"stream_id"`
		StreamSequence uint64                   `json:"stream_sequence"`
		TeamID         orchestration.TeamID     `json:"team_id"`
		Type           orchestration.EventType  `json:"type"`
		OccurredAt     string                   `json:"occurred_at"`
		ActorAgentID   orchestration.AgentID    `json:"actor_agent_id"`
		CausationID    string                   `json:"causation_id"`
		CorrelationID  string                   `json:"correlation_id"`
		IdempotencyKey string                   `json:"idempotency_key"`
		Payload        json.RawMessage          `json:"payload"`
		SchemaVersion  uint32                   `json:"schema_version"`
	}{event.EventID, event.GlobalSequence, event.StreamType, event.StreamID, event.StreamSequence,
		event.TeamID, event.Type, event.OccurredAt.UTC().Format(time.RFC3339Nano), event.ActorAgentID,
		event.CausationID, event.CorrelationID, event.IdempotencyKey, event.Payload, event.SchemaVersion}
	data, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	return sha256Hex(data), nil
}

// Append writes an orchestration event batch and its sequence counters in one authority
// transaction.
func (store *Store) Append(ctx context.Context, stream orchestration.Stream, expected uint64, pending ...orchestration.PendingEvent) (result []orchestration.Event, err error) {
	if err = stream.Validate(); err != nil {
		return nil, err
	}
	if len(pending) == 0 {
		return nil, nil
	}
	err = store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		var e error
		result, e = appendEvents(tx, stream, expected, pending...)
		return e
	})
	return
}

func (store *Store) loadEvents(ctx context.Context, kind, scope string, after uint64, limit int) (result []orchestration.Event, err error) {
	err = store.authority.View(ctx, func(tx *ateauthority.Tx) error {
		var last uint64
		counter := scope
		if kind == "global" {
			counter = "global"
		}
		if _, err := tx.Get(recordKey("event-"+kind+"-counter", counter), &last); err != nil {
			return err
		}
		cursor := after
		if kind == "team" {
			// Public Team cursors are global positions. Binary search the
			// Team's dense native index without reading unrelated event bodies.
			low, high := uint64(0), last
			for low < high {
				mid := low + (high-low)/2 + 1
				index, err := readEventIndex(tx, kind, scope, mid)
				if err != nil {
					return err
				}
				if index.Global <= after {
					low = mid
				} else {
					high = mid - 1
				}
			}
			cursor = low
		}
		result = make([]orchestration.Event, 0)
		for cursor < last && len(result) < boundedLimit(limit) {
			cursor++
			event, err := readIndexedEvent(tx, kind, scope, cursor)
			if err != nil {
				return err
			}
			result = append(result, event)
		}
		return nil
	})
	return
}

func (store *Store) LoadStream(ctx context.Context, kind orchestration.StreamType, id string, after uint64, limit int) ([]orchestration.Event, error) {
	if kind == "" || id == "" {
		return nil, fmt.Errorf("%w: stream type and ID are required", orchestration.ErrInvalidEvent)
	}
	return store.loadEvents(ctx, "stream", streamCounterID(kind, id), after, limit)
}
func (store *Store) LoadGlobal(ctx context.Context, after orchestration.Cursor, limit int) ([]orchestration.Event, error) {
	return store.loadEvents(ctx, "global", "", uint64(after), limit)
}
func (store *Store) LoadTeam(ctx context.Context, id orchestration.TeamID, after orchestration.Cursor, limit int) ([]orchestration.Event, error) {
	if err := id.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", orchestration.ErrInvalidEvent, err)
	}
	return store.loadEvents(ctx, "team", string(id), uint64(after), limit)
}

// CurrentSequence reads the current counter for the specified orchestration stream.
func (store *Store) CurrentSequence(ctx context.Context, kind orchestration.StreamType, id string) (result uint64, err error) {
	if kind == "" || id == "" {
		return 0, fmt.Errorf("%w: stream type and ID are required", orchestration.ErrInvalidEvent)
	}
	err = store.authority.View(ctx, func(tx *ateauthority.Tx) error {
		_, e := tx.Get(recordKey("event-stream-counter", streamCounterID(kind, id)), &result)
		return e
	})
	return
}

var _ orchestration.EventStore = (*Store)(nil)

// Package atestore persists orchestration entities as individual ATE records.
//
// Every repository mutation and its events share one authority transaction.
// Native transaction size limits are propagated; atomic operations are never
// split to fit them. ATE request errors retain their recovery identity.
//
// Integration boundary: this repository does not write the legacy SQL temporal
// outbox. Event queries seek native sequence indexes; entity enumeration uses
// native prefix pages. Neither path retains host projections.
package atestore

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

type Store struct {
	authority *ateauthority.Store
	sessions  session.EventStore
}

// New constructs the orchestration repository over the supplied ATE authority.
func New(authority *ateauthority.Store, events session.EventStore) (*Store, error) {
	if authority == nil {
		return nil, fmt.Errorf("orchestration ATE authority is required")
	}
	return &Store{authority: authority, sessions: events}, nil
}

// recordKey namespaces the record kind and base64url-encodes the ID for unambiguous prefix
// scans.
func recordKey(kind, id string) string {
	return "gsea/v1/" + kind + "/" + base64.RawURLEncoding.EncodeToString([]byte(id))
}

// getRecord decodes a typed record from the current transaction and wraps ErrNotFound when
// the key is absent.
func getRecord[T any](tx *ateauthority.Tx, kind, id string) (T, error) {
	var value T
	var raw json.RawMessage
	found, err := tx.Get(recordKey(kind, id), &raw)
	if err != nil {
		return value, err
	}
	if !found {
		return value, fmt.Errorf("%w: %s %q", orchestration.ErrNotFound, kind, id)
	}
	return decodeRecord[T](raw)
}

// putRecord stages the encoded record under its kind and ID in the caller's transaction.
func putRecord(tx *ateauthority.Tx, kind, id string, value any) error {
	return tx.Put(recordKey(kind, id), encodeRecord(value))
}

// createRecord stages a new record using the transaction's create-only precondition.
func createRecord(tx *ateauthority.Tx, kind, id string, value any) error {
	return tx.Create(recordKey(kind, id), encodeRecord(value))
}

// RawMessage is compacted by encoding/json. SQL mailbox idempotency compares
// original bytes, so keep those bytes in the same individual message record.
type messageRecord struct {
	orchestration.Message
	OriginalBody []byte `json:"original_body"`
}

type eventRecord struct {
	orchestration.Event
	OriginalPayload []byte `json:"original_payload"`
}

func encodeRecord(value any) any {
	switch message := value.(type) {
	case orchestration.Message:
		return messageRecord{Message: message, OriginalBody: []byte(message.Body)}
	case *orchestration.Message:
		if message != nil {
			return messageRecord{Message: *message, OriginalBody: []byte(message.Body)}
		}
	case orchestration.Event:
		return eventRecord{Event: message, OriginalPayload: []byte(message.Payload)}
	}
	return value
}

func decodeRecord[T any](raw json.RawMessage) (T, error) {
	var value T
	if event, ok := any(&value).(*orchestration.Event); ok {
		var record eventRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			return value, err
		}
		if record.OriginalPayload == nil || !json.Valid(record.OriginalPayload) {
			return value, fmt.Errorf("%w: missing original event payload", orchestration.ErrInvalidEvent)
		}
		*event = record.Event
		event.Payload = json.RawMessage(record.OriginalPayload)
		return value, nil
	}
	if message, ok := any(&value).(*orchestration.Message); ok {
		var record messageRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			return value, err
		}
		if record.OriginalBody == nil || !json.Valid(record.OriginalBody) {
			return value, fmt.Errorf("%w: missing original mailbox body", orchestration.ErrInvalidEvent)
		}
		*message = record.Message
		message.Body = json.RawMessage(record.OriginalBody)
		return value, nil
	}
	err := json.Unmarshal(raw, &value)
	return value, err
}

// listRecords scans one record-kind prefix and decodes its values in the current
// transaction.
func listRecords[T any](tx *ateauthority.Tx, kind string) ([]T, error) {
	values := make([]T, 0)
	err := tx.List(recordKey(kind, ""), func(key string, raw json.RawMessage) error {
		value, err := decodeRecord[T](raw)
		if err != nil {
			return fmt.Errorf("decode %s: %w", key, err)
		}
		values = append(values, value)
		return nil
	})
	return values, err
}

// streamCounterID combines stream type and ID for the per-stream sequence counter.
func streamCounterID(kind orchestration.StreamType, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(kind)) + "/" + base64.RawURLEncoding.EncodeToString([]byte(id))
}

// appendEvents checks the expected stream sequence and stages events with their stream,
// team and global counters in the same transaction.
func appendEvents(tx *ateauthority.Tx, stream orchestration.Stream, expected uint64, pending ...orchestration.PendingEvent) ([]orchestration.Event, error) {
	if err := stream.Validate(); err != nil {
		return nil, err
	}
	for _, item := range pending {
		if err := item.Validate(); err != nil {
			return nil, err
		}
	}
	var current, global, teamSequence uint64
	streamID := streamCounterID(stream.Type, stream.ID)
	if _, err := tx.Get(recordKey("event-stream-counter", streamID), &current); err != nil {
		return nil, err
	}
	if expected != orchestration.AnySequence && expected != current {
		return nil, fmt.Errorf("%w: expected %d, current %d", orchestration.ErrSequenceConflict, expected, current)
	}
	if _, err := tx.Get(recordKey("event-global-counter", "global"), &global); err != nil {
		return nil, err
	}
	if _, err := tx.Get(recordKey("event-team-counter", string(stream.TeamID)), &teamSequence); err != nil {
		return nil, err
	}
	if uint64(len(pending)) > ^uint64(0)-current || uint64(len(pending)) > ^uint64(0)-global || uint64(len(pending)) > ^uint64(0)-teamSequence {
		return nil, fmt.Errorf("%w: event sequence overflow", orchestration.ErrInvalidEvent)
	}
	now := time.Now().UTC()
	result := make([]orchestration.Event, 0, len(pending))
	for i, item := range pending {
		id, err := orchestration.NewEventID()
		if err != nil {
			return nil, err
		}
		event := orchestration.Event{
			EventID: id, GlobalSequence: global + uint64(i) + 1,
			StreamType: stream.Type, StreamID: stream.ID, StreamSequence: current + uint64(i) + 1,
			TeamID: stream.TeamID, Type: item.Type, OccurredAt: now.Add(time.Duration(i)),
			ActorAgentID: item.ActorAgentID, CausationID: item.CausationID, CorrelationID: item.CorrelationID,
			IdempotencyKey: item.IdempotencyKey, Payload: append(json.RawMessage(nil), item.Payload...), SchemaVersion: 1,
		}
		event.Checksum, err = orchestrationChecksum(event)
		if err != nil {
			return nil, err
		}
		if err := createRecord(tx, "event", string(id), event); err != nil {
			return nil, err
		}
		if err := indexEvent(tx, event, teamSequence+uint64(i)+1); err != nil {
			return nil, err
		}
		result = append(result, event)
	}
	if len(pending) > 0 {
		if err := putRecord(tx, "event-stream-counter", streamID, current+uint64(len(pending))); err != nil {
			return nil, err
		}
		if err := putRecord(tx, "event-global-counter", "global", global+uint64(len(pending))); err != nil {
			return nil, err
		}
		if err := putRecord(tx, "event-team-counter", string(stream.TeamID), teamSequence+uint64(len(pending))); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func boundedLimit(limit int) int {
	if limit <= 0 {
		return 1000
	}
	return min(limit, 10000)
}

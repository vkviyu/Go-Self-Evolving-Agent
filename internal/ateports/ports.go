// Package ateports defines the host's ports onto native ATE data: exact source
// history reads and semantic decision state. It is not a delivery journal, an
// outbox, or a second business store, and it depends on no runtime or engine.
package ateports

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

const (
	StreamSession       = "session"
	StreamOrchestration = "orchestration"
	DefaultLimit        = 1000
	MaxLimit            = 10_000
)

var (
	ErrNotFound = errors.New("native ATE record not found")
	ErrConflict = errors.New("native ATE state conflict")
	ErrInvalid  = errors.New("invalid native ATE port input")
)

type (
	// Record is a transient read of exactly one native source event, with no
	// delivery-queue identity. Payload is the JSON encoding of the entire
	// original event, including identity and checksum.
	// Sequence is the session sequence or orchestration global sequence.
	// SessionID is empty for orchestration events; their context is in Payload.
	Record struct {
		Stream     string
		SourceID   string
		SessionID  string
		Sequence   uint64
		Kind       string
		Payload    json.RawMessage
		OccurredAt time.Time
	}

	// StateStore persists decision and checkpoint JSON in the native authority,
	// independently of source history and pending commands. It must not serve
	// as a source-event or acknowledged-command mirror.
	// Put inserts once, allowing identical bytes
	// and rejecting conflicting bytes with ErrConflict. Keys must be nonempty
	// and values must be valid JSON.
	// Implementations may additionally expose CompareAndSwapStore.
	StateStore interface {
		Get(ctx context.Context, key string) (json.RawMessage, bool, error)
		// Put creates a JSON value at key. Repeating identical bytes is allowed, while
		// different bytes conflict.
		Put(ctx context.Context, key string, value json.RawMessage) error
	}

	// HistoryReader reads authoritative original session events, including
	// acknowledged records, ordered by sequence strictly greater than after.
	// Record preserves the complete source envelope; delivery status does not
	// determine source completeness. Reads must return a consistent page and
	// report unavailable or corrupt history rather than silently omitting it.
	// Limits default to DefaultLimit and are capped at MaxLimit.
	// Readers supporting frozen, paginated cuts also implement HistoryCutStore.
	HistoryReader interface {
		// History returns an ordered page of source events strictly after the supplied
		// session sequence.
		History(ctx context.Context, sessionID string, after uint64, limit int) ([]Record, error)
	}

	// HistoryCutStore freezes a session history high-water mark and reads
	// pages through that mark. Implementations must reject corrupted or
	// out-of-range records rather than silently returning an incomplete cut.
	HistoryCutStore interface {
		HistoryCut(ctx context.Context, sessionID string) (uint64, error)
		HistoryThrough(ctx context.Context, sessionID string, after, through uint64, limit int) ([]Record, error)
	}

	// CompareAndSwapStore changes an existing value only when its bytes match
	// expected. Use Put to create a key. A mismatch or absent key is ErrConflict.
	CompareAndSwapStore interface {
		CompareAndSwap(ctx context.Context, key string, expected, value json.RawMessage) error
	}
)

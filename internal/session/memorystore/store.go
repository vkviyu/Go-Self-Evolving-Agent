// Package memorystore provides a deterministic event store for tests and ephemeral sessions.
package memorystore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

// Store is an in-memory implementation of session.EventStore and
// session.CheckpointStore.
type Store struct {
	mu          sync.RWMutex
	events      map[session.ID][]session.Event
	checkpoints map[string][]byte
}

// New creates an empty store.
func New() *Store {
	return &Store{events: make(map[session.ID][]session.Event), checkpoints: make(map[string][]byte)}
}

// Append atomically appends events to one in-memory session.
func (s *Store) Append(ctx context.Context, sessionID session.ID, expected uint64, pending ...session.PendingEvent) ([]session.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if sessionID == "" {
		return nil, fmt.Errorf("%w: session ID is required", session.ErrInvalidEvent)
	}
	for _, event := range pending {
		if err := event.Validate(); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current := uint64(len(s.events[sessionID]))
	if expected != session.AnySequence && expected != current {
		return nil, fmt.Errorf("%w: expected %d, current %d", session.ErrSequenceConflict, expected, current)
	}
	appended := make([]session.Event, 0, len(pending))
	for index, item := range pending {
		id, err := randomID()
		if err != nil {
			return nil, err
		}
		event := session.Event{
			SessionID:         sessionID,
			Sequence:          current + uint64(index) + 1,
			EventID:           id,
			Type:              item.Type,
			OccurredAt:        time.Now().UTC(),
			CausationID:       item.CausationID,
			CorrelationID:     item.CorrelationID,
			RuntimeSnapshotID: item.RuntimeSnapshotID,
			Payload:           append([]byte(nil), item.Payload...),
			SchemaVersion:     1,
			Checksum:          "memory-store",
		}
		appended = append(appended, event)
	}
	s.events[sessionID] = append(s.events[sessionID], appended...)
	return cloneEvents(appended), nil
}

// Load returns a copy of events after the requested sequence.
func (s *Store) Load(ctx context.Context, sessionID session.ID, after uint64, limit int) ([]session.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 1000
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	all := s.events[sessionID]
	start := min(int(after), len(all))
	end := min(start+limit, len(all))
	return cloneEvents(all[start:end]), nil
}

// CurrentSequence returns the number of events in a session.
func (s *Store) CurrentSequence(ctx context.Context, sessionID session.ID) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return uint64(len(s.events[sessionID])), nil
}

func (s *Store) LoadByCorrelation(ctx context.Context, sessionID session.ID, correlationID string) ([]session.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	matched := make([]session.Event, 0)
	for _, event := range s.events[sessionID] {
		if event.CorrelationID == correlationID {
			matched = append(matched, event)
		}
	}
	return cloneEvents(matched), nil
}

// Close is a no-op.
func (s *Store) Close() error { return nil }

func (s *Store) Get(ctx context.Context, checkpointID string) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, ok := s.checkpoints[checkpointID]
	return append([]byte(nil), data...), ok, nil
}

func (s *Store) Set(ctx context.Context, checkpointID string, checkpoint []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if checkpointID == "" || len(checkpoint) == 0 {
		return fmt.Errorf("checkpoint ID and data are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.checkpoints == nil {
		s.checkpoints = make(map[string][]byte)
	}
	s.checkpoints[checkpointID] = append([]byte(nil), checkpoint...)
	return nil
}

func (s *Store) Delete(ctx context.Context, checkpointID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.checkpoints, checkpointID)
	return nil
}

func randomID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func cloneEvents(events []session.Event) []session.Event {
	result := make([]session.Event, len(events))
	copy(result, events)
	for index := range result {
		result[index].Payload = append([]byte(nil), result[index].Payload...)
	}
	return result
}

var _ session.EventStore = (*Store)(nil)
var _ session.CorrelatedEventStore = (*Store)(nil)
var _ session.CheckpointStore = (*Store)(nil)

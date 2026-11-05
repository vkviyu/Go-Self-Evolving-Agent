package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

// sessionEventColumns is the column list scanSessionEvent reads.
const sessionEventColumns = `sequence, event_id, event_type, occurred_at, causation_id, correlation_id, runtime_snapshot_id, payload, checksum, schema_version`

type SessionEventStore struct {
	db *sql.DB
}

// Append atomically appends events after optionally checking the current sequence.
func (s *SessionEventStore) Append(
	ctx context.Context,
	sessionID session.ID,
	expectedSequence uint64,
	pending ...session.PendingEvent,
) ([]session.Event, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("%w: session ID is required", session.ErrInvalidEvent)
	}
	if len(pending) == 0 {
		return nil, nil
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin session append: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	appended, err := appendSessionEvents(ctx, tx, sessionID, expectedSequence, pending...)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit session append: %w", err)
	}
	return appended, nil
}

// appendSessionEvents shares the event codec/sequence contract with atomic
// offline recovery decisions. The caller owns the transaction.
func appendSessionEvents(ctx context.Context, tx *sql.Tx, sessionID session.ID, expectedSequence uint64, pending ...session.PendingEvent) ([]session.Event, error) {
	if sessionID == "" {
		return nil, session.ErrInvalidEvent
	}
	for _, event := range pending {
		if err := event.Validate(); err != nil {
			return nil, err
		}
	}
	current, err := currentSequence(ctx, tx, sessionID)
	if err != nil {
		return nil, err
	}
	if expectedSequence != session.AnySequence && current != expectedSequence {
		return nil, fmt.Errorf("%w: expected %d, current %d", session.ErrSequenceConflict, expectedSequence, current)
	}

	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO session_events(
    session_id, sequence, event_id, event_type, occurred_at,
    causation_id, correlation_id, runtime_snapshot_id,
    payload, checksum, schema_version
) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return nil, fmt.Errorf("prepare session append: %w", err)
	}
	defer stmt.Close()

	now := time.Now().UTC()
	appended := make([]session.Event, 0, len(pending))
	for index, item := range pending {
		eventID, err := randomID()
		if err != nil {
			return nil, err
		}
		event := session.Event{
			SessionID:         sessionID,
			Sequence:          current + uint64(index) + 1,
			EventID:           eventID,
			Type:              item.Type,
			OccurredAt:        now.Add(time.Duration(index) * time.Nanosecond),
			CausationID:       item.CausationID,
			CorrelationID:     item.CorrelationID,
			RuntimeSnapshotID: item.RuntimeSnapshotID,
			Payload:           cloneBytes(item.Payload),
			SchemaVersion:     sessionEventSchemaVersion,
		}
		event.Checksum, err = checksum(event)
		if err != nil {
			return nil, err
		}
		if _, err := stmt.ExecContext(
			ctx,
			string(event.SessionID), event.Sequence, event.EventID, string(event.Type), event.OccurredAt.Format(time.RFC3339Nano),
			event.CausationID, event.CorrelationID, event.RuntimeSnapshotID,
			[]byte(event.Payload), event.Checksum, event.SchemaVersion,
		); err != nil {
			return nil, fmt.Errorf("insert session event %q: %w", event.Type, err)
		}
		appended = append(appended, event)
	}

	return appended, nil
}

// Load returns events in ascending sequence order and verifies their checksums.
func (s *SessionEventStore) Load(ctx context.Context, sessionID session.ID, afterSequence uint64, limit int) ([]session.Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sessionEventColumns+` FROM session_events
WHERE session_id = ? AND sequence > ?
ORDER BY sequence ASC
LIMIT ?`, string(sessionID), afterSequence, boundedLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("load session events: %w", err)
	}
	return collectSessionEvents(rows, sessionID, "iterate session events")
}

// CurrentSequence returns zero for a session with no events.
func (s *SessionEventStore) CurrentSequence(ctx context.Context, sessionID session.ID) (uint64, error) {
	return currentSequence(ctx, s.db, sessionID)
}

// LoadByCorrelation uses the session/correlation indexable prefix to recover a
// single durable tool outcome. Results retain checksum verification.
func (s *SessionEventStore) LoadByCorrelation(ctx context.Context, sessionID session.ID, correlationID string) ([]session.Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sessionEventColumns+` FROM session_events
WHERE session_id = ? AND correlation_id = ?
ORDER BY sequence ASC`, string(sessionID), correlationID)
	if err != nil {
		return nil, fmt.Errorf("load correlated session events: %w", err)
	}
	return collectSessionEvents(rows, sessionID, "iterate correlated session events")
}

var _ session.EventStore = (*SessionEventStore)(nil)
var _ session.CorrelatedEventStore = (*SessionEventStore)(nil)

// collectSessionEvents scans and closes rows selected with
// sessionEventColumns; iterateContext wraps a late iteration error.
func collectSessionEvents(rows *sql.Rows, sessionID session.ID, iterateContext string) ([]session.Event, error) {
	defer rows.Close()
	events := make([]session.Event, 0)
	for rows.Next() {
		event, err := scanSessionEvent(rows, sessionID)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", iterateContext, err)
	}
	return events, nil
}

// scanSessionEvent reads one row selected with sessionEventColumns and
// verifies its checksum.
func scanSessionEvent(scanner rowScanner, sessionID session.ID) (session.Event, error) {
	var event session.Event
	var eventType, occurredAt string
	var payload []byte
	event.SessionID = sessionID
	if err := scanner.Scan(
		&event.Sequence, &event.EventID, &eventType, &occurredAt,
		&event.CausationID, &event.CorrelationID, &event.RuntimeSnapshotID,
		&payload, &event.Checksum, &event.SchemaVersion,
	); err != nil {
		return session.Event{}, fmt.Errorf("scan session event: %w", err)
	}
	event.Type = session.EventType(eventType)
	event.Payload = cloneBytes(payload)
	var err error
	event.OccurredAt, err = time.Parse(time.RFC3339Nano, occurredAt)
	if err != nil {
		return session.Event{}, fmt.Errorf("parse event time: %w", err)
	}
	actual, err := checksum(event)
	if err != nil {
		return session.Event{}, err
	}
	if actual != event.Checksum {
		return session.Event{}, fmt.Errorf("session event %s checksum mismatch", event.EventID)
	}
	return event, nil
}

func currentSequence(ctx context.Context, q rowQuery, sessionID session.ID) (uint64, error) {
	var current uint64
	if err := q.QueryRowContext(ctx, `
SELECT COALESCE(MAX(sequence), 0) FROM session_events WHERE session_id = ?`, string(sessionID)).Scan(&current); err != nil {
		return 0, fmt.Errorf("read session sequence: %w", err)
	}
	return current, nil
}

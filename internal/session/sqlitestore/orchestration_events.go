package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

func (store *OrchestrationStore) Append(
	ctx context.Context,
	stream orchestration.Stream,
	expectedSequence uint64,
	pending ...orchestration.PendingEvent,
) ([]orchestration.Event, error) {
	if err := stream.Validate(); err != nil {
		return nil, err
	}
	if len(pending) == 0 {
		return nil, nil
	}
	for _, event := range pending {
		if err := event.Validate(); err != nil {
			return nil, err
		}
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin orchestration append: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	appended, err := appendOrchestrationEvents(ctx, tx, stream, expectedSequence, pending...)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit orchestration append: %w", err)
	}
	return appended, nil
}

func appendOrchestrationEvents(
	ctx context.Context,
	tx *sql.Tx,
	stream orchestration.Stream,
	expectedSequence uint64,
	pending ...orchestration.PendingEvent,
) ([]orchestration.Event, error) {
	current, err := currentOrchestrationSequence(ctx, tx, stream.Type, stream.ID)
	if err != nil {
		return nil, err
	}
	if expectedSequence != orchestration.AnySequence && current != expectedSequence {
		return nil, fmt.Errorf("%w: expected %d, current %d", orchestration.ErrSequenceConflict, expectedSequence, current)
	}
	var global uint64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(global_sequence), 0) FROM orchestration_events`).Scan(&global); err != nil {
		return nil, fmt.Errorf("read orchestration global sequence: %w", err)
	}
	statement, err := tx.PrepareContext(ctx, `
INSERT INTO orchestration_events(
    global_sequence, event_id, stream_type, stream_id, stream_sequence,
    team_id, event_type, occurred_at, actor_agent_id, causation_id,
    correlation_id, idempotency_key, payload, checksum, schema_version
) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return nil, fmt.Errorf("prepare orchestration append: %w", err)
	}
	defer statement.Close()

	now := time.Now().UTC()
	appended := make([]orchestration.Event, 0, len(pending))
	for index, item := range pending {
		eventID, err := orchestration.NewEventID()
		if err != nil {
			return nil, err
		}
		event := orchestration.Event{
			EventID:        eventID,
			GlobalSequence: global + uint64(index) + 1,
			StreamType:     stream.Type,
			StreamID:       stream.ID,
			StreamSequence: current + uint64(index) + 1,
			TeamID:         stream.TeamID,
			Type:           item.Type,
			OccurredAt:     now.Add(time.Duration(index) * time.Nanosecond),
			ActorAgentID:   item.ActorAgentID,
			CausationID:    item.CausationID,
			CorrelationID:  item.CorrelationID,
			IdempotencyKey: item.IdempotencyKey,
			Payload:        cloneBytes(item.Payload),
			SchemaVersion:  orchestrationEventSchemaVersion,
		}
		event.Checksum, err = orchestrationChecksum(event)
		if err != nil {
			return nil, err
		}
		if _, err := statement.ExecContext(ctx,
			event.GlobalSequence, string(event.EventID), string(event.StreamType), event.StreamID, event.StreamSequence,
			string(event.TeamID), string(event.Type), event.OccurredAt.Format(time.RFC3339Nano), string(event.ActorAgentID),
			event.CausationID, event.CorrelationID, event.IdempotencyKey, []byte(event.Payload), event.Checksum, event.SchemaVersion,
		); err != nil {
			return nil, fmt.Errorf("insert orchestration event %q: %w", event.Type, err)
		}
		appended = append(appended, event)
	}
	return appended, nil
}

func (store *OrchestrationStore) LoadStream(
	ctx context.Context,
	streamType orchestration.StreamType,
	streamID string,
	afterSequence uint64,
	limit int,
) ([]orchestration.Event, error) {
	if streamType == "" || streamID == "" {
		return nil, fmt.Errorf("%w: stream type and ID are required", orchestration.ErrInvalidEvent)
	}
	return store.load(ctx, `
SELECT global_sequence, event_id, stream_type, stream_id, stream_sequence,
       team_id, event_type, occurred_at, actor_agent_id, causation_id,
       correlation_id, idempotency_key, payload, checksum, schema_version
FROM orchestration_events
WHERE stream_type = ? AND stream_id = ? AND stream_sequence > ?
ORDER BY stream_sequence ASC
LIMIT ?`, streamType, streamID, afterSequence, boundedLimit(limit))
}

func (store *OrchestrationStore) LoadGlobal(ctx context.Context, after orchestration.Cursor, limit int) ([]orchestration.Event, error) {
	return store.load(ctx, `
SELECT global_sequence, event_id, stream_type, stream_id, stream_sequence,
       team_id, event_type, occurred_at, actor_agent_id, causation_id,
       correlation_id, idempotency_key, payload, checksum, schema_version
FROM orchestration_events
WHERE global_sequence > ?
ORDER BY global_sequence ASC
LIMIT ?`, uint64(after), boundedLimit(limit))
}

// LoadTeam pages the global event order of one Team. The Team filter runs
// before the limit, so a page is never emptied by other Teams' events.
func (store *OrchestrationStore) LoadTeam(ctx context.Context, teamID orchestration.TeamID, after orchestration.Cursor, limit int) ([]orchestration.Event, error) {
	if err := teamID.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", orchestration.ErrInvalidEvent, err)
	}
	return store.load(ctx, `
SELECT global_sequence, event_id, stream_type, stream_id, stream_sequence,
       team_id, event_type, occurred_at, actor_agent_id, causation_id,
       correlation_id, idempotency_key, payload, checksum, schema_version
FROM orchestration_events
WHERE team_id = ? AND global_sequence > ?
ORDER BY global_sequence ASC
LIMIT ?`, string(teamID), uint64(after), boundedLimit(limit))
}

func (store *OrchestrationStore) load(ctx context.Context, query string, arguments ...any) ([]orchestration.Event, error) {
	rows, err := store.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("load orchestration events: %w", err)
	}
	defer rows.Close()
	events := make([]orchestration.Event, 0)
	for rows.Next() {
		event, err := scanOrchestrationEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate orchestration events: %w", err)
	}
	return events, nil
}

func scanOrchestrationEvent(scanner rowScanner) (orchestration.Event, error) {
	var event orchestration.Event
	var eventID, streamType, teamID, eventType, actorAgentID, occurredAt string
	var payload []byte
	if err := scanner.Scan(
		&event.GlobalSequence, &eventID, &streamType, &event.StreamID, &event.StreamSequence,
		&teamID, &eventType, &occurredAt, &actorAgentID, &event.CausationID,
		&event.CorrelationID, &event.IdempotencyKey, &payload, &event.Checksum, &event.SchemaVersion,
	); err != nil {
		return orchestration.Event{}, fmt.Errorf("scan orchestration event: %w", err)
	}
	event.EventID = orchestration.EventID(eventID)
	event.StreamType = orchestration.StreamType(streamType)
	event.TeamID = orchestration.TeamID(teamID)
	event.Type = orchestration.EventType(eventType)
	event.ActorAgentID = orchestration.AgentID(actorAgentID)
	event.Payload = cloneBytes(payload)
	var err error
	event.OccurredAt, err = time.Parse(time.RFC3339Nano, occurredAt)
	if err != nil {
		return orchestration.Event{}, fmt.Errorf("parse orchestration event time: %w", err)
	}
	if err := event.EventID.Validate(); err != nil {
		return orchestration.Event{}, fmt.Errorf("validate orchestration event ID: %w", err)
	}
	if err := event.TeamID.Validate(); err != nil {
		return orchestration.Event{}, fmt.Errorf("validate orchestration team ID: %w", err)
	}
	actual, err := orchestrationChecksum(event)
	if err != nil {
		return orchestration.Event{}, err
	}
	if actual != event.Checksum {
		return orchestration.Event{}, fmt.Errorf("orchestration event %s checksum mismatch", event.EventID)
	}
	return event, nil
}

func (store *OrchestrationStore) CurrentSequence(ctx context.Context, streamType orchestration.StreamType, streamID string) (uint64, error) {
	if streamType == "" || streamID == "" {
		return 0, fmt.Errorf("%w: stream type and ID are required", orchestration.ErrInvalidEvent)
	}
	return currentOrchestrationSequence(ctx, store.db, streamType, streamID)
}

func currentOrchestrationSequence(
	ctx context.Context,
	query rowQuery,
	streamType orchestration.StreamType,
	streamID string,
) (uint64, error) {
	var current uint64
	if err := query.QueryRowContext(ctx, `
SELECT COALESCE(MAX(stream_sequence), 0)
FROM orchestration_events
WHERE stream_type = ? AND stream_id = ?`, string(streamType), streamID).Scan(&current); err != nil {
		return 0, fmt.Errorf("read orchestration stream sequence: %w", err)
	}
	return current, nil
}

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
	}{
		EventID:        event.EventID,
		GlobalSequence: event.GlobalSequence,
		StreamType:     event.StreamType,
		StreamID:       event.StreamID,
		StreamSequence: event.StreamSequence,
		TeamID:         event.TeamID,
		Type:           event.Type,
		OccurredAt:     event.OccurredAt.UTC().Format(time.RFC3339Nano),
		ActorAgentID:   event.ActorAgentID,
		CausationID:    event.CausationID,
		CorrelationID:  event.CorrelationID,
		IdempotencyKey: event.IdempotencyKey,
		Payload:        event.Payload,
		SchemaVersion:  event.SchemaVersion,
	}
	data, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("checksum orchestration event: %w", err)
	}
	return sha256Hex(data), nil
}

func boundedLimit(limit int) int {
	if limit <= 0 {
		return 1000
	}
	return min(limit, 10_000)
}

var _ orchestration.EventStore = (*OrchestrationStore)(nil)

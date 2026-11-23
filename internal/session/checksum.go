package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// EventChecksum binds the complete event envelope independently of its storage.
// An ATE reader and a runtime reader must agree on the same evidence identity.
func EventChecksum(event Event) (string, error) {
	canonical := struct {
		SessionID         ID              `json:"session_id"`
		Sequence          uint64          `json:"sequence"`
		EventID           string          `json:"event_id"`
		Type              EventType       `json:"type"`
		OccurredAt        string          `json:"occurred_at"`
		CausationID       string          `json:"causation_id"`
		CorrelationID     string          `json:"correlation_id"`
		RuntimeSnapshotID string          `json:"runtime_snapshot_id"`
		Payload           json.RawMessage `json:"payload"`
		SchemaVersion     uint32          `json:"schema_version"`
	}{
		SessionID: event.SessionID, Sequence: event.Sequence, EventID: event.EventID,
		Type: event.Type, OccurredAt: event.OccurredAt.UTC().Format(time.RFC3339Nano),
		CausationID: event.CausationID, CorrelationID: event.CorrelationID,
		RuntimeSnapshotID: event.RuntimeSnapshotID, Payload: event.Payload,
		SchemaVersion: event.SchemaVersion,
	}
	raw, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("checksum session event: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

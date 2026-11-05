package sqlitestore

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

// randomID returns a 128-bit random hex identifier for events, reservations
// and other store-generated records.
func randomID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate random ID: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func checksum(event session.Event) (string, error) {
	canonical := struct {
		SessionID         session.ID        `json:"session_id"`
		Sequence          uint64            `json:"sequence"`
		EventID           string            `json:"event_id"`
		Type              session.EventType `json:"type"`
		OccurredAt        string            `json:"occurred_at"`
		CausationID       string            `json:"causation_id"`
		CorrelationID     string            `json:"correlation_id"`
		RuntimeSnapshotID string            `json:"runtime_snapshot_id"`
		Payload           json.RawMessage   `json:"payload"`
		SchemaVersion     uint32            `json:"schema_version"`
	}{
		SessionID:         event.SessionID,
		Sequence:          event.Sequence,
		EventID:           event.EventID,
		Type:              event.Type,
		OccurredAt:        event.OccurredAt.UTC().Format(time.RFC3339Nano),
		CausationID:       event.CausationID,
		CorrelationID:     event.CorrelationID,
		RuntimeSnapshotID: event.RuntimeSnapshotID,
		Payload:           event.Payload,
		SchemaVersion:     event.SchemaVersion,
	}
	data, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("checksum session event: %w", err)
	}
	return sha256Hex(data), nil
}

// sha256Hex is the hex-encoded SHA-256 digest used for every stored checksum
// and request hash.
func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func cloneBytes[T ~[]byte](value T) T {
	if value == nil {
		return nil
	}
	return append(T(nil), value...)
}

// Package retention defines explicit offline lifecycle closure, independently
// of executable registries and UI. A released consumer is not a deletion proof.
package retention

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

var ErrPlanChanged = errors.New("retention plan changed; preview again before confirming")
var ErrBlocked = errors.New("retention is blocked by unfinished or unverifiable consumers")

type Consumer struct {
	Reference  tool.SnapshotReference `json:"reference"`
	SnapshotID string                 `json:"snapshot_id,omitempty"`
}

type Plan struct {
	ID        string     `json:"plan_id"`
	SessionID session.ID `json:"session_id"`
	Sequence  uint64     `json:"session_sequence"`
	Consumers []Consumer `json:"consumers"`
	Blockers  []string   `json:"blockers"`
}

func (plan Plan) Digest() string {
	plan.ID = ""
	encoded, _ := json.Marshal(plan)
	return Digest(encoded)
}

// Digest returns the hexadecimal SHA-256 fingerprint of the supplied bytes.
func Digest(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

type Receipt struct {
	Plan               Plan      `json:"plan"`
	ClosedAt           time.Time `json:"closed_at"`
	ReleasedReferences int       `json:"released_references"`
	EvidenceDeleted    bool      `json:"evidence_deleted"`
}

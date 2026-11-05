package session

import "context"

// CheckpointStore persists opaque Eino execution state. It is deliberately
// separate from Store: checkpoint bytes are framework state, not session facts
// and must never be projected into model history.
type CheckpointStore interface {
	Get(ctx context.Context, checkpointID string) ([]byte, bool, error)
	Set(ctx context.Context, checkpointID string, checkpoint []byte) error
	Delete(ctx context.Context, checkpointID string) error
}

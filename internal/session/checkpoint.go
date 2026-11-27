package session

import "context"

// CheckpointStore persists opaque Eino execution state. It is deliberately
// separate from Store: checkpoint bytes are framework state, not session facts
// and must never be projected into model history.
type CheckpointStore interface {
	// Get loads checkpoint bytes and reports whether the requested checkpoint exists.
	Get(ctx context.Context, checkpointID string) ([]byte, bool, error)
	// Set persists opaque framework checkpoint bytes under the supplied checkpoint ID.
	Set(ctx context.Context, checkpointID string, checkpoint []byte) error
	// Delete removes the checkpoint identified by checkpointID.
	Delete(ctx context.Context, checkpointID string) error
}

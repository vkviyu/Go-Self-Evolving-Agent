package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

type CheckpointStore struct {
	db *sql.DB
}

// Get loads opaque framework checkpoint state.
func (s *CheckpointStore) Get(ctx context.Context, checkpointID string) ([]byte, bool, error) {
	if checkpointID == "" {
		return nil, false, errors.New("checkpoint ID is required")
	}
	var data []byte
	var storedChecksum string
	err := s.db.QueryRowContext(ctx, `SELECT checkpoint, checksum FROM agent_checkpoints WHERE checkpoint_id = ?`, checkpointID).Scan(&data, &storedChecksum)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("load checkpoint: %w", err)
	}
	if actual := sha256Hex(data); actual != storedChecksum {
		return nil, false, fmt.Errorf("checkpoint %q checksum mismatch", checkpointID)
	}
	return append([]byte(nil), data...), true, nil
}

// Set atomically replaces opaque framework checkpoint state.
func (s *CheckpointStore) Set(ctx context.Context, checkpointID string, checkpoint []byte) error {
	if checkpointID == "" {
		return errors.New("checkpoint ID is required")
	}
	if len(checkpoint) == 0 {
		return errors.New("checkpoint data is empty")
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO agent_checkpoints(checkpoint_id, checkpoint, checksum, updated_at) VALUES(?, ?, ?, ?)
ON CONFLICT(checkpoint_id) DO UPDATE SET checkpoint = excluded.checkpoint, checksum = excluded.checksum, updated_at = excluded.updated_at`,
		checkpointID, checkpoint, sha256Hex(checkpoint), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("save checkpoint: %w", err)
	}
	return nil
}

// Delete removes a completed or discarded checkpoint.
func (s *CheckpointStore) Delete(ctx context.Context, checkpointID string) error {
	if checkpointID == "" {
		return errors.New("checkpoint ID is required")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM agent_checkpoints WHERE checkpoint_id = ?`, checkpointID); err != nil {
		return fmt.Errorf("delete checkpoint: %w", err)
	}
	return nil
}

var _ session.CheckpointStore = (*CheckpointStore)(nil)

package runtimestore

import (
	"context"
	"fmt"
)

// Compact returns already-freed SQLite pages to the filesystem. It makes no
// retention decisions, deletes no runtime resource, and requires the offline
// owner because compaction rewrites the file.
func (database *Database) Compact(ctx context.Context) error {
	var integrity string
	if err := database.db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		return err
	}
	if integrity != "ok" {
		return fmt.Errorf("refuse to compact database: %s", integrity)
	}
	if _, err := database.db.ExecContext(ctx, `VACUUM`); err != nil {
		return err
	}
	var busy, log, checkpointed int
	if err := database.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &log, &checkpointed); err != nil {
		return err
	}
	if busy != 0 {
		return fmt.Errorf("compaction completed but WAL checkpoint is busy")
	}
	return nil
}

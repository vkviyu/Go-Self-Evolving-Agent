// Package runtimestore owns the embedded database for host runtime resources:
// checkpoints, workspace-facing runtime snapshots and pending ATE request
// envelopes. ATE owns business history, tasks, messages and usage.
package runtimestore

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"
)

type Database struct {
	db        *sql.DB
	closeOnce sync.Once
	closeErr  error
}

func (database *Database) Checkpoints() *CheckpointStore {
	return &CheckpointStore{db: database.db}
}

func (database *Database) Close() error {
	if database == nil {
		return nil
	}
	database.closeOnce.Do(func() {
		if err := database.db.Close(); err != nil {
			database.closeErr = fmt.Errorf("close sqlite runtime database: %w", err)
		}
	})
	return database.closeErr
}

func dataSourceName(path string) (string, error) {
	if path == ":memory:" {
		identifier, err := randomID()
		if err != nil {
			return "", err
		}
		return "file:local-agent-runtime-" + identifier + "?mode=memory&cache=shared&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)&_txlock=immediate&_defensive=1", nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve sqlite path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return "", fmt.Errorf("create sqlite directory: %w", err)
	}
	u := &url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	query := u.Query()
	query.Add("_pragma", "foreign_keys(ON)")
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "journal_mode(WAL)")
	query.Set("_defensive", "1")
	// Serialize read/compare/write transactions across separately opened handles.
	query.Set("_txlock", "immediate")
	u.RawQuery = query.Encode()
	return u.String(), nil
}

// Package sqlitestore owns the embedded database used by durable runtime stores.
package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"
)

const (
	sessionEventSchemaVersion       uint32 = 1
	orchestrationEventSchemaVersion uint32 = 1
)

type Database struct {
	db        *sql.DB
	closeOnce sync.Once
	closeErr  error
}

func Open(ctx context.Context, path string) (*Database, error) {
	if path == "" {
		return nil, errors.New("sqlite path is required")
	}
	dsn, err := dataSourceName(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite runtime database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	database := &Database{db: db}
	if err := database.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return database, nil
}

func (database *Database) SessionEvents() *SessionEventStore {
	return &SessionEventStore{db: database.db}
}

func (database *Database) Checkpoints() *CheckpointStore {
	return &CheckpointStore{db: database.db}
}

func (database *Database) Orchestration() *OrchestrationStore {
	return &OrchestrationStore{db: database.db}
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
		return "file:local-agent-session-" + identifier + "?mode=memory&cache=shared&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)&_defensive=1", nil
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
	u.RawQuery = query.Encode()
	return u.String(), nil
}

// Package sessionregistry stores the user-facing Agent Session catalog.
// Conversation history remains in each session's runtime database; this
// registry only maps a stable Session identity to its managed resources.
package sessionregistry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type State string

var ErrNotFound = errors.New("session does not exist")

const (
	StateCreated State = "created"
	StateRunning State = "running"
	StateStopped State = "stopped"
	StateFailed  State = "failed"
)

// sessionColumns is the agent_sessions projection read by scanSession.
const sessionColumns = `session_id,name,workspace_directory,runtime_directory,data_directory,
       temp_directory,tool_state_directory,state,pid,model,provider,created_at,updated_at`

type Session struct {
	ID            string
	Name          string
	Workspace     string
	Runtime       string
	DataDirectory string
	TempDirectory string
	ToolState     string
	State         State
	PID           int
	Model         string
	Provider      string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type Registry struct {
	db *sql.DB
}

func Open(ctx context.Context, path string) (*Registry, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("session registry path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve session registry path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		return nil, fmt.Errorf("create session registry directory: %w", err)
	}
	db, err := sql.Open("sqlite", absolute)
	if err != nil {
		return nil, fmt.Errorf("open session registry: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	registry := &Registry{db: db}
	if _, err := db.ExecContext(ctx, `
PRAGMA busy_timeout = 5000;
PRAGMA foreign_keys = ON;
PRAGMA journal_mode = WAL;
CREATE TABLE IF NOT EXISTS agent_sessions (
    session_id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    workspace_directory TEXT NOT NULL,
    runtime_directory TEXT NOT NULL,
    data_directory TEXT NOT NULL,
    temp_directory TEXT NOT NULL,
    tool_state_directory TEXT NOT NULL,
    state TEXT NOT NULL,
    pid INTEGER NOT NULL DEFAULT 0,
    model TEXT NOT NULL DEFAULT '',
    provider TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
)`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize session registry: %w", err)
	}
	return registry, nil
}

func (registry *Registry) Close() error {
	if registry == nil || registry.db == nil {
		return nil
	}
	return registry.db.Close()
}

func (registry *Registry) Create(ctx context.Context, item Session) error {
	if registry == nil || registry.db == nil {
		return errors.New("session registry is unavailable")
	}
	if strings.TrimSpace(item.ID) == "" || strings.TrimSpace(item.Name) == "" {
		return errors.New("session ID and name are required")
	}
	if strings.TrimSpace(item.Workspace) == "" || strings.TrimSpace(item.Runtime) == "" {
		return errors.New("session workspace and runtime directory are required")
	}
	if item.State == "" {
		item.State = StateCreated
	}
	if !validState(item.State) || item.PID < 0 {
		return errors.New("invalid session state or PID")
	}
	tx, err := registry.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	if item.CreatedAt.IsZero() {
		item.CreatedAt = now
	}
	if item.UpdatedAt.IsZero() {
		item.UpdatedAt = now
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO agent_sessions(
    session_id,name,workspace_directory,runtime_directory,data_directory,
    temp_directory,tool_state_directory,state,pid,model,provider,created_at,updated_at
) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		item.ID, item.Name, item.Workspace, item.Runtime, item.DataDirectory,
		item.TempDirectory, item.ToolState, item.State, item.PID, item.Model,
		item.Provider, item.CreatedAt.Format(time.RFC3339Nano), item.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("create session %q: %w", item.ID, err)
	}
	var created []string
	committed := false
	defer func() {
		if !committed {
			for index := len(created) - 1; index >= 0; index-- {
				_ = os.Remove(created[index])
			}
		}
	}()
	for _, directory := range []string{item.Runtime, item.DataDirectory, item.TempDirectory, item.ToolState} {
		if strings.TrimSpace(directory) == "" {
			continue
		}
		if err := createResourceDirectory(directory, &created); err != nil {
			return fmt.Errorf("create session resources: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

func (registry *Registry) Get(ctx context.Context, id string) (Session, error) {
	if registry == nil || registry.db == nil {
		return Session{}, errors.New("session registry is unavailable")
	}
	item, err := scanSession(registry.db.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM agent_sessions WHERE session_id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	if err != nil {
		return Session{}, fmt.Errorf("read session %q: %w", id, err)
	}
	return item, nil
}

func (registry *Registry) List(ctx context.Context) ([]Session, error) {
	if registry == nil || registry.db == nil {
		return nil, errors.New("session registry is unavailable")
	}
	rows, err := registry.db.QueryContext(ctx, `SELECT `+sessionColumns+` FROM agent_sessions ORDER BY created_at ASC, session_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	var result []Session
	for rows.Next() {
		item, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sessions: %w", err)
	}
	return result, nil
}

// scanSession decodes one sessionColumns row from either *sql.Row or *sql.Rows.
func scanSession(row interface{ Scan(...any) error }) (Session, error) {
	var item Session
	var state, createdAt, updatedAt string
	if err := row.Scan(
		&item.ID, &item.Name, &item.Workspace, &item.Runtime, &item.DataDirectory,
		&item.TempDirectory, &item.ToolState, &state, &item.PID, &item.Model,
		&item.Provider, &createdAt, &updatedAt); err != nil {
		return Session{}, err
	}
	item.State = State(state)
	var err error
	if item.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return Session{}, fmt.Errorf("parse session %q creation time: %w", item.ID, err)
	}
	if item.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return Session{}, fmt.Errorf("parse session %q update time: %w", item.ID, err)
	}
	return item, nil
}

func (registry *Registry) UpdateState(ctx context.Context, id string, state State, pid int) error {
	if registry == nil || registry.db == nil {
		return errors.New("session registry is unavailable")
	}
	if !validState(state) || pid < 0 || (state != StateRunning && pid != 0) {
		return errors.New("invalid session state or PID")
	}
	result, err := registry.db.ExecContext(ctx, `
UPDATE agent_sessions SET state = ?, pid = ?, updated_at = ? WHERE session_id = ? AND (? != 'created' OR state = 'created')`,
		state, pid, time.Now().UTC().Format(time.RFC3339Nano), id, state)
	if err != nil {
		return fmt.Errorf("update session %q state: %w", id, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		if _, err := registry.Get(ctx, id); err != nil {
			return err
		}
		return errors.New("cannot reset an existing session to created")
	}
	return nil
}

func (registry *Registry) Remove(ctx context.Context, id string) error {
	if registry == nil || registry.db == nil {
		return errors.New("session registry is unavailable")
	}
	result, err := registry.db.ExecContext(ctx, `DELETE FROM agent_sessions WHERE session_id = ? AND state != 'running'`, id)
	if err != nil {
		return fmt.Errorf("remove session %q: %w", id, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		if _, err := registry.Get(ctx, id); err != nil {
			return err
		}
		return errors.New("cannot remove a running Session")
	}
	return nil
}

func validState(state State) bool {
	return state == StateCreated || state == StateRunning || state == StateStopped || state == StateFailed
}

// Remove only new, empty directories if registration fails. Never recursively
// delete caller data or change permissions of a preexisting resource tree.
func createResourceDirectory(path string, created *[]string) error {
	if info, err := os.Stat(path); err == nil {
		if !info.IsDir() {
			return errors.New("session resource is not a directory")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return errors.New("session resource has no existing parent")
	}
	if err := createResourceDirectory(parent, created); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0700); err != nil {
		if errors.Is(err, os.ErrExist) {
			info, statErr := os.Stat(path)
			if statErr == nil && info.IsDir() {
				return nil
			}
		}
		return err
	}
	*created = append(*created, path)
	return nil
}

// ReconcileStopped repairs a stale running projection after uncatchable
// process death. A live/reused PID is conservatively kept running.
func (registry *Registry) ReconcileStopped(ctx context.Context, id string) (Session, error) {
	item, err := registry.Get(ctx, id)
	if err != nil || item.State != StateRunning {
		return item, err
	}
	alive, err := processAlive(item.PID)
	if err != nil || alive {
		return item, err
	}
	_, err = registry.db.ExecContext(ctx, `UPDATE agent_sessions SET state='stopped',pid=0,updated_at=? WHERE session_id=? AND state='running' AND pid=?`, time.Now().UTC().Format(time.RFC3339Nano), id, item.PID)
	if err != nil {
		return Session{}, err
	}
	return registry.Get(ctx, id)
}

package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/lsp"
)

// LSPProjectionStore is the durable read model for diagnostics. Language
// server process state remains owned by lsp.Manager.
type LSPProjectionStore struct {
	db *sql.DB
}

func (database *Database) LSPDiagnostics() *LSPProjectionStore {
	if database == nil {
		return nil
	}
	return &LSPProjectionStore{db: database.db}
}

func (store *LSPProjectionStore) SaveProjection(ctx context.Context, projection lsp.Projection) error {
	if store == nil || store.db == nil {
		return errors.New("LSP diagnostic projection store is unavailable")
	}
	if strings.TrimSpace(projection.WorkspaceRoot) == "" || strings.TrimSpace(projection.Path) == "" || strings.TrimSpace(projection.Revision) == "" {
		return errors.New("LSP diagnostic projection requires workspace, path and revision")
	}
	report, err := json.Marshal(projection.Report)
	if err != nil {
		return err
	}
	updatedAt := projection.UpdatedAt.UTC()
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	_, err = store.db.ExecContext(ctx, `
INSERT INTO lsp_diagnostic_projections(workspace_root, path, revision, report, updated_at)
VALUES(?, ?, ?, ?, ?)
ON CONFLICT(workspace_root, path, revision) DO UPDATE SET
    report = excluded.report,
    updated_at = excluded.updated_at`,
		projection.WorkspaceRoot, projection.Path, projection.Revision, report, updatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("save LSP diagnostic projection: %w", err)
	}
	return nil
}

func (store *LSPProjectionStore) LoadProjection(ctx context.Context, workspaceRoot, path, revision string) (lsp.Projection, bool, error) {
	if store == nil || store.db == nil {
		return lsp.Projection{}, false, errors.New("LSP diagnostic projection store is unavailable")
	}
	projection := lsp.Projection{WorkspaceRoot: workspaceRoot, Path: path, Revision: revision}
	var report []byte
	var updatedAt string
	err := store.db.QueryRowContext(ctx, `
SELECT report, updated_at
FROM lsp_diagnostic_projections
WHERE workspace_root = ? AND path = ? AND revision = ?`, workspaceRoot, path, revision).Scan(&report, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return lsp.Projection{}, false, nil
	}
	if err != nil {
		return lsp.Projection{}, false, fmt.Errorf("load LSP diagnostic projection: %w", err)
	}
	if err := json.Unmarshal(report, &projection.Report); err != nil {
		return lsp.Projection{}, false, fmt.Errorf("decode LSP diagnostic projection: %w", err)
	}
	projection.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return lsp.Projection{}, false, fmt.Errorf("parse LSP diagnostic projection time: %w", err)
	}
	return projection, true, nil
}

var _ lsp.ProjectionStore = (*LSPProjectionStore)(nil)

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/lsp"
	"github.com/vkviyu/go-self-evolving-agent/internal/sandbox"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

// RuntimeSnapshotReferences lists unreleased consumer references, not runnable
// tasks or proof of deletion safety. There is no automatic reference release.
func (a *App) RuntimeSnapshotReferences(ctx context.Context, identity string) ([]tool.SnapshotReference, error) {
	if a == nil {
		return nil, errors.New("application snapshot store is unavailable")
	}
	a.stateMu.Lock()
	snapshots, closed := a.runtime.snapshots, a.closed
	a.stateMu.Unlock()
	if snapshots == nil || closed {
		return nil, errors.New("application snapshot store is unavailable")
	}
	return snapshots.References(ctx, identity)
}

// RuntimeSnapshotReferenceHistory includes explicitly released references and
// their reasons. Abandonment retains this evidence instead of deleting rows.
func (a *App) RuntimeSnapshotReferenceHistory(ctx context.Context, identity string) ([]tool.SnapshotReferenceRecord, error) {
	if a == nil {
		return nil, errors.New("application snapshot store is unavailable")
	}
	a.stateMu.Lock()
	snapshots, closed := a.runtime.snapshots, a.closed
	a.stateMu.Unlock()
	if snapshots == nil || closed {
		return nil, errors.New("application snapshot store is unavailable")
	}
	return snapshots.ReferenceHistory(ctx, identity)
}

// Native executors contain closures over runtime services. Their host binary
// alone is insufficient identity: changing workspace or sandbox must never
// redirect a recovered operation. Only the digest enters snapshot manifests.
func nativeSnapshotScope(resolved config.Config, workspaceRoot string, mode sandbox.Mode) (string, error) {
	// Recovery may obtain the data directory from the Session catalog, which
	// publishes its physical path. Use the same database identity when the
	// initial launch entered through a symlink.
	database, err := filepath.EvalSymlinks(resolved.RuntimeDatabasePath())
	if err != nil {
		return "", fmt.Errorf("resolve runtime database identity: %w", err)
	}
	encoded, err := json.Marshal(struct {
		Version             int
		Workspace, Database string
		Sandbox             sandbox.Mode
		LSP                 lsp.Config
		Model               config.ModelConfig
	}{1, workspaceRoot, database, mode, resolved.LSP, resolved.Model})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return "host-scope-v1:" + hex.EncodeToString(digest[:]), nil
}

func scopeNativeBindings(scope string, bindings []tool.Binding) []tool.Binding {
	scoped := append([]tool.Binding(nil), bindings...)
	for index := range scoped {
		scoped[index].ImplementationID = scope + ":" + scoped[index].ImplementationID
	}
	return scoped
}

package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

// SessionReconcileResult contains metadata only, never saved request contents.
type SessionReconcileResult struct {
	SessionID session.ID `json:"session_id"`
	Outcome   string     `json:"outcome"`
}

var reconcileSessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ReconcileSession recovers saved Session plans without constructing executable
// services. cfg selects the existing runtime database and authority explicitly;
// catalog registration is not required. Success does not resume any Agent turn.
func ReconcileSession(ctx context.Context, cfg config.Config, id session.ID) (out SessionReconcileResult, resultErr error) {
	if !reconcileSessionIDPattern.MatchString(string(id)) {
		return out, errors.New("reconcile requires a valid Session ID")
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	cfg, err := sessionReconcileConfig(cfg)
	if err != nil {
		return out, err
	}
	a, err := OpenControl(ctx, cfg)
	if err != nil {
		return out, err
	}
	defer func() { resultErr = errors.Join(resultErr, a.Close()) }()
	a.stateMu.Lock()
	err = a.ownSession(id)
	a.stateMu.Unlock()
	if err != nil {
		return out, err
	}
	err = withOfflineDatabase(ctx, cfg, func(database *runtimestore.Database) error {
		pending := database.ATERequests()
		if err := pending.BindAuthority(ctx, a.authority.storeID); err != nil {
			return err
		}
		a.stateMu.Lock()
		defer a.stateMu.Unlock()
		if err := a.controlRepositories(pending); err != nil {
			return err
		}
		// This startup helper derives child scope from authoritative membership
		// and acquires every child lease before advancing any saved plan.
		if err := a.recoverRootSessionRequests(ctx, id); err != nil {
			return err
		}
		// An authoritative root can exist before its first history append.
		// History-only interactive Sessions need no orchestration root.
		if _, err := a.repositories.RootThreadBySession(ctx, id); err == nil {
			return nil
		} else if !errors.Is(err, orchestration.ErrNotFound) {
			return err
		}
		return a.authority.bound.WithClient(ctx, func(c *client.Client) error {
			stored, err := c.GetSession(ctx, api.ReadSessionRequest{
				Space: authoritySpace, Principal: "gsea", SessionID: string(id),
			})
			if err != nil {
				return err
			}
			if !stored.Found {
				return errors.New("Session was not found in the selected authority")
			}
			return nil
		})
	})
	if err != nil {
		return out, err
	}
	return SessionReconcileResult{SessionID: id, Outcome: "reconciled"}, nil
}

// Check before Config.Resolve, runtimeowner.Acquire or OpenControl: their normal
// startup paths may create missing directories, databases or an empty authority.
func sessionReconcileConfig(cfg config.Config) (config.Config, error) {
	if !cfg.ATE.Enabled {
		return cfg, errors.New("session reconcile requires ate.enabled")
	}
	if cfg.DataDirectory == "" {
		cfg.DataDirectory = cfg.HomeDirectory
	}
	if cfg.WorkspaceDirectory == "" {
		var err error
		cfg.WorkspaceDirectory, err = os.Getwd()
		if err != nil {
			return cfg, errors.New("session reconcile requires an existing workspace")
		}
	}
	if cfg.ATE.Directory == "" {
		base := cfg.HomeDirectory
		if base == "" {
			base = cfg.DataDirectory
		}
		cfg.ATE.Directory = filepath.Join(base, "temporal")
	}
	for _, path := range []string{cfg.DataDirectory, cfg.WorkspaceDirectory, cfg.ATE.Directory} {
		if strings.TrimSpace(path) == "" || len(path) > 4096 || strings.ContainsAny(path, "\x00\r\n") {
			return cfg, errors.New("session reconcile paths must be nonblank, bounded and contain no NUL or line breaks")
		}
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			return cfg, errors.New("session reconcile requires existing data, workspace and ATE directories; check configuration and path overrides")
		}
	}
	info, err := os.Stat(cfg.RuntimeDatabasePath())
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return cfg, errors.New("session reconcile requires an existing runtime database; check --data-dir")
	}
	directory, err := os.Open(cfg.ATE.Directory)
	if err != nil {
		return cfg, errors.New("session reconcile cannot inspect the existing ATE directory")
	}
	names, readErr := directory.Readdirnames(1)
	closeErr := directory.Close()
	if len(names) == 0 || (readErr != nil && !errors.Is(readErr, io.EOF)) || closeErr != nil {
		return cfg, errors.New("session reconcile requires an existing nonempty ATE store")
	}
	return cfg.Resolve()
}

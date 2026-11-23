package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/sessionregistry"
)

// OpenSessionRegistry opens the native ATE catalog without opening an agent
// runtime. The local database holds only transient native request envelopes;
// it is never queried for session metadata or history.
func OpenSessionRegistry(ctx context.Context, cfg config.Config) (sessionregistry.Catalog, error) {
	resources, closeFn, err := openCatalogAuthority(ctx, cfg)
	if err != nil {
		return nil, err
	}
	registry, err := sessionregistry.NewAuthority(resources.records, closeFn)
	if err != nil {
		return nil, errors.Join(err, closeFn())
	}
	return registry, nil
}

// SessionRegistry returns a catalog view over this App's already-open
// authority. The App owns the underlying ATE process and remains responsible
// for closing it; the returned catalog only owns its wrapper.
func (a *App) SessionRegistry() (sessionregistry.Catalog, error) {
	if a == nil || a.authority == nil || a.authority.records == nil {
		return nil, errors.New("application session authority is unavailable")
	}
	return sessionregistry.NewAuthority(a.authority.records, nil)
}

func openCatalogAuthority(ctx context.Context, cfg config.Config) (*authorityResources, func() error, error) {
	if !cfg.ATE.Enabled {
		return nil, nil, errors.New("ATE is required for session and task persistence; enable ate.enabled")
	}
	if strings.TrimSpace(cfg.HomeDirectory) == "" {
		return nil, nil, errors.New("session catalog home directory is required")
	}
	home, err := filepath.Abs(cfg.HomeDirectory)
	if err != nil {
		return nil, nil, err
	}
	// Catalog reads do not need a live workspace or a runtime data directory.
	// Leave those paths alone; the process manager validates the ATE options.
	cfg.HomeDirectory = home
	if cfg.ATE.Directory == "" {
		cfg.ATE.Directory = filepath.Join(home, "temporal")
	}
	pending, err := runtimestore.OpenRuntime(ctx, filepath.Join(home, "authority-runtime-v3.db"))
	if err != nil {
		return nil, nil, err
	}
	resources, err := openAuthority(ctx, cfg, pending.ATERequests())
	if err != nil {
		return nil, nil, errors.Join(err, pending.Close())
	}
	closeFn := func() error {
		return errors.Join(resources.process.Close(), pending.Close())
	}
	return resources, closeFn, nil
}

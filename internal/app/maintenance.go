package app

import (
	"context"
	"errors"

	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/retention"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimeowner"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/sqlitestore"
)

// Maintenance owns only the durable database, never executable registries,
// model clients or a recovering Supervisor. Keep this boundary offline.
func withOfflineDatabase(ctx context.Context, cfg config.Config, run func(*sqlitestore.Database) error) (result error) {
	resolved, err := cfg.Resolve()
	if err != nil {
		return err
	}
	owner, err := runtimeowner.Acquire(resolved.SessionDatabasePath())
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, owner.Close()) }()
	database, err := sqlitestore.Open(ctx, resolved.SessionDatabasePath())
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, database.Close()) }()
	return run(database)
}

func PreviewRetention(ctx context.Context, cfg config.Config, id session.ID) (plan retention.Plan, err error) {
	err = withOfflineDatabase(ctx, cfg, func(database *sqlitestore.Database) error {
		var err error
		plan, err = database.RetentionPlan(ctx, id)
		return err
	})
	return
}

func ArchiveSession(ctx context.Context, cfg config.Config, id session.ID, planID string, confirmed bool) (receipt retention.Receipt, err error) {
	err = withOfflineDatabase(ctx, cfg, func(database *sqlitestore.Database) error {
		var err error
		receipt, err = database.CloseRetention(ctx, id, planID, confirmed)
		return err
	})
	return
}

func PreviewPluginCollection(ctx context.Context, cfg config.Config, name, revision string) (plan retention.CollectionPlan, err error) {
	err = withOfflineDatabase(ctx, cfg, func(database *sqlitestore.Database) error {
		var err error
		plan, err = database.PluginCollectionPlan(ctx, name, revision)
		return err
	})
	return
}

func CollectPlugin(ctx context.Context, cfg config.Config, name, revision, planID string, confirmed bool) (receipt retention.CollectionReceipt, err error) {
	err = withOfflineDatabase(ctx, cfg, func(database *sqlitestore.Database) error {
		var err error
		receipt, err = database.CollectPlugin(ctx, name, revision, planID, confirmed)
		return err
	})
	return
}

func CompactDatabase(ctx context.Context, cfg config.Config, confirmed bool) error {
	if !confirmed {
		return errors.New("database compaction requires explicit confirmation")
	}
	return withOfflineDatabase(ctx, cfg, func(database *sqlitestore.Database) error { return database.Compact(ctx) })
}

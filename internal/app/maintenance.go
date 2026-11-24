package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	orchestrationate "github.com/vkviyu/go-self-evolving-agent/internal/orchestration/atestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/retention"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimeowner"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

// Maintenance owns only the durable database, never executable registries,
// model clients or a recovering Supervisor. Keep this boundary offline.
func withOfflineDatabase(ctx context.Context, cfg config.Config, run func(*runtimestore.Database) error) (result error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	resolved, err := cfg.Resolve()
	if err != nil {
		return err
	}
	owner, err := runtimeowner.Acquire(resolved.RuntimeDatabasePath())
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, owner.Close()) }()
	database, err := runtimestore.OpenRuntime(ctx, resolved.RuntimeDatabasePath())
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, database.Close()) }()
	return run(database)
}

func PreviewRetention(ctx context.Context, cfg config.Config, id session.ID) (plan retention.Plan, result error) {
	result = withOfflineSessionAuthority(ctx, cfg, id, func(store *orchestrationate.Store, database *runtimestore.Database) error {
		local, err := database.RuntimeRetentionReferences(ctx, id)
		if err != nil {
			return err
		}
		plan, err = store.RetentionPlan(ctx, id, local)
		return err
	})
	return
}

func ArchiveSession(ctx context.Context, cfg config.Config, id session.ID, planID string, confirmed bool) (receipt retention.Receipt, result error) {
	if !confirmed || planID == "" || id == "" {
		return receipt, errors.New("archive requires an explicitly confirmed retention plan")
	}
	result = withOfflineSessionAuthority(ctx, cfg, id, func(store *orchestrationate.Store, database *runtimestore.Database) error {
		local, err := database.RuntimeRetentionReferences(ctx, id)
		if err != nil {
			return err
		}
		receipt, err = store.CloseRetention(ctx, id, planID, confirmed, local)
		if err != nil {
			return err
		}
		receipt.ReleasedReferences, err = database.CleanupRetention(ctx, receipt)
		if err != nil {
			return fmt.Errorf("archive facts are durable; retry plan %s for runtime cleanup: %w", planID, err)
		}
		return nil
	})
	return
}

func CompactDatabase(ctx context.Context, cfg config.Config, confirmed bool) error {
	if !confirmed {
		return errors.New("database compaction requires explicit confirmation")
	}
	return withOfflineDatabase(ctx, cfg, func(database *runtimestore.Database) error { return database.Compact(ctx) })
}

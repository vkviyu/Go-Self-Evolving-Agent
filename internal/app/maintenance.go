package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

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

func PreviewPluginCollection(ctx context.Context, cfg config.Config, name, revision string) (plan retention.CollectionPlan, result error) {
	result = withOfflinePluginAuthority(ctx, cfg, func(_ *orchestrationate.Store, database *runtimestore.Database, resource string) error {
		var err error
		plan, err = database.RuntimePluginCollectionPlan(ctx, name, revision)
		if err == nil {
			plan.ID = orchestrationate.CollectionPlanID(resource, plan)
		}
		return err
	})
	return
}

func CollectPlugin(ctx context.Context, cfg config.Config, name, revision, planID string, confirmed bool) (receipt retention.CollectionReceipt, result error) {
	if !confirmed || name == "" || revision == "" || planID == "" {
		return receipt, errors.New("collection requires exact revision, reviewed plan ID and explicit confirmation")
	}
	result = withOfflinePluginAuthority(ctx, cfg, func(authority *orchestrationate.Store, database *runtimestore.Database, resource string) error {
		stored, found, err := authority.PluginCollectionReceipt(ctx, resource, planID)
		if err != nil {
			return err
		}
		plan := stored.Plan
		if found {
			if plan.Tool != name || plan.Revision != revision {
				return retention.ErrPlanChanged
			}
		} else {
			plan, err = database.RuntimePluginCollectionPlan(ctx, name, revision)
			if err != nil {
				return err
			}
			plan.ID = orchestrationate.CollectionPlanID(resource, plan)
			if plan.ID != planID {
				return retention.ErrPlanChanged
			}
		}
		receipt, err = authority.PreparePluginCollection(ctx, resource, plan, confirmed)
		if err != nil {
			return err
		}
		receipt, err = database.CleanupPluginCollection(ctx, receipt)
		if err != nil {
			return fmt.Errorf("collection intent is durable; retry local cleanup with plan %s: %w", planID, err)
		}
		if err := authority.CompletePluginCollection(ctx, resource, receipt); err != nil {
			return fmt.Errorf("local collection completed; retry native receipt finalization with plan %s: %w", planID, err)
		}
		return nil
	})
	return
}

// Artifacts belong to one runtime resource. The runtimeowner database lock
// excludes all local executors throughout inspection, native audit and cleanup.
// Reuse scoped offline request reconciliation with a reserved resource lock;
// this does not create a business Session or grant global revision ownership.
func withOfflinePluginAuthority(ctx context.Context, cfg config.Config, run func(*orchestrationate.Store, *runtimestore.Database, string) error) error {
	resolved, err := cfg.Resolve()
	if err != nil {
		return err
	}
	scope := session.ID("plugin-resource:" + retention.Digest([]byte(resolved.RuntimeDatabasePath())))
	return withOfflineSessionAuthority(ctx, resolved, scope, func(authority *orchestrationate.Store, database *runtimestore.Database) error {
		path, err := filepath.EvalSymlinks(resolved.RuntimeDatabasePath())
		if err != nil {
			return err
		}
		return run(authority, database, "runtime-db:"+retention.Digest([]byte(path)))
	})
}

func CompactDatabase(ctx context.Context, cfg config.Config, confirmed bool) error {
	if !confirmed {
		return errors.New("database compaction requires explicit confirmation")
	}
	return withOfflineDatabase(ctx, cfg, func(database *runtimestore.Database) error { return database.Compact(ctx) })
}

package app

import (
	"context"

	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/sqlitestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

// ReadSessionUsage reads the entire durable team ledger after the runtime has
// stopped. Like offline maintenance, it acquires ownership before opening the
// database; it never starts models, recovery or executable registries.
func ReadSessionUsage(ctx context.Context, cfg config.Config, id session.ID) (report tokenusage.UsageReport, err error) {
	err = withOfflineDatabase(ctx, cfg, func(database *sqlitestore.Database) error {
		store := database.Orchestration()
		root, err := store.RootThreadBySession(ctx, id)
		if err != nil {
			return err
		}
		report, err = store.UsageReport(ctx, tokenusage.UsageQuery{TeamID: root.TeamID})
		return err
	})
	return
}

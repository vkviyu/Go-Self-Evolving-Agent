package app

import (
	"context"

	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	orchestrationate "github.com/vkviyu/go-self-evolving-agent/internal/orchestration/atestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

type sessionUsageReader interface {
	RootThreadBySession(context.Context, session.ID) (orchestration.RootThread, error)
	tokenusage.Reader
}

// ReadSessionUsage reads the authoritative team ledger without constructing
// models, a Supervisor, or executable registries.
func ReadSessionUsage(ctx context.Context, cfg config.Config, id session.ID) (report tokenusage.UsageReport, err error) {
	err = withOfflineDatabase(ctx, cfg, func(database *runtimestore.Database) error {
		resolved, err := cfg.Resolve()
		if err != nil {
			return err
		}
		authority, err := openAuthority(ctx, resolved, database.ATERequests())
		if err != nil {
			return err
		}
		defer authority.process.Close()
		repositories, err := orchestrationate.New(authority.records, authority.sessions)
		if err != nil {
			return err
		}
		report, err = readSessionUsage(ctx, repositories, id)
		return err
	})
	return
}

func readSessionUsage(ctx context.Context, store sessionUsageReader, id session.ID) (tokenusage.UsageReport, error) {
	root, err := store.RootThreadBySession(ctx, id)
	if err != nil {
		return tokenusage.UsageReport{}, err
	}
	return store.UsageReport(ctx, tokenusage.UsageQuery{TeamID: root.TeamID})
}

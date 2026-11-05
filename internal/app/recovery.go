package app

import (
	"context"

	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/conversation"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/sqlitestore"
)

// Recoveries is an offline read boundary: it neither constructs a model nor
// starts the Supervisor (whose recovery may run delegated work).
func (a *App) Recoveries(ctx context.Context, id session.ID) ([]conversation.Recovery, error) {
	return conversation.DiscoverRecoveries(ctx, id, a.events, a.checkpoints, a.database.Orchestration())
}

// AbandonRecovery is intentionally separate from App: no model, plugin
// runtime or Supervisor is constructed, and another live owner must be closed.
func AbandonRecovery(ctx context.Context, unresolved config.Config, request session.AbandonRequest) (receipt session.Abandonment, result error) {
	if err := request.Validate(); err != nil {
		return receipt, err
	}
	result = withOfflineDatabase(ctx, unresolved, func(database *sqlitestore.Database) error {
		var err error
		receipt, err = database.AbandonRecovery(ctx, request)
		return err
	})
	return
}

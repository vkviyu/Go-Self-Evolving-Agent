package app

import (
	"context"
	"fmt"

	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/conversation"
	orchestrationate "github.com/vkviyu/go-self-evolving-agent/internal/orchestration/atestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

// Recoveries is an offline read boundary: it neither constructs a model nor
// starts the Supervisor (whose recovery may run delegated work).
func (a *App) Recoveries(ctx context.Context, id session.ID) ([]conversation.Recovery, error) {
	return conversation.DiscoverRecoveries(ctx, id, a.events, a.runtime.checkpoints)
}

// AbandonRecovery is intentionally separate from App: no model, plugin
// runtime or Supervisor is constructed. withOfflineSessionAuthority acquires
// ownSession's StoreID+Session lock before reconciliation and holds it through
// both authoritative mutation and all local reference/checkpoint cleanup.
func AbandonRecovery(ctx context.Context, unresolved config.Config, request session.AbandonRequest) (receipt session.Abandonment, result error) {
	if err := request.Validate(); err != nil {
		return receipt, err
	}
	result = withOfflineSessionAuthority(ctx, unresolved, request.SessionID, func(repositories *orchestrationate.Store, database *runtimestore.Database) error {
		var err error
		receipt, err = abandonRecoveryWithStores(ctx, request, repositories, database.RuntimeSnapshots(), database.Checkpoints())
		return err
	})
	return
}

type abandonmentAuthority interface {
	AbandonRecovery(context.Context, session.AbandonRequest) (session.Abandonment, error)
}

type abandonedReferences interface {
	CloseAbandonedReferences(context.Context, session.Abandonment) (int, error)
}

// Cleanup is deliberately after the complete authoritative receipt. An error
// returns that receipt and leaves the operation retryable; it never attempts to
// roll back authoritative facts or claims that external effects were reverted.
func abandonRecoveryWithStores(ctx context.Context, request session.AbandonRequest, authority abandonmentAuthority, references abandonedReferences, checkpoints session.CheckpointStore) (session.Abandonment, error) {
	receipt, err := authority.AbandonRecovery(ctx, request)
	if err != nil {
		return session.Abandonment{}, err
	}
	released, err := references.CloseAbandonedReferences(ctx, receipt)
	if err != nil {
		return receipt, fmt.Errorf("abandonment is durable; retry runtime reference cleanup: %w", err)
	}
	receipt.ReleasedReferences = released
	for _, turnID := range receipt.TurnIDs {
		// Matches the runtime checkpoint identity; opaque checkpoints are not
		// used as business proof. Session events, candidates and reports remain.
		id := "session/" + string(receipt.SessionID) + "/turn/" + turnID
		if err := checkpoints.Delete(ctx, id); err != nil {
			return receipt, fmt.Errorf("abandonment is durable; retry checkpoint cleanup for %q: %w", turnID, err)
		}
	}
	return receipt, nil
}

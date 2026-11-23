package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	sessionate "github.com/vkviyu/go-self-evolving-agent/internal/session/atestore"
)

// RecoverSessionRequests is the execution-owner boundary for saved Session
// plans. Call after attaching the Session and before reading runtime history or
// resuming maintenance. Read-only control startup must not call it.
func (a *App) RecoverSessionRequests(ctx context.Context, id session.ID) error {
	if a == nil {
		return errors.New("application authority is unavailable")
	}
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	return a.recoverSessionRequests(ctx, id)
}

// recoverSessionRequests requires stateMu. Holding it prevents Close from
// releasing the Session lease while the native plan is being recovered.
func (a *App) recoverSessionRequests(ctx context.Context, id session.ID) error {
	if a.closed || a.authority == nil || a.authority.sessions == nil || a.sessionOwners[id] == nil {
		return errors.New("pending session recovery requires its execution-owner lease")
	}
	if err := a.authority.sessions.RecoverSession(ctx, id); err != nil {
		return fmt.Errorf("recover owned session %s requests: %w", id, err)
	}
	return nil
}

// offlineSessionPending.BindAuthority has already acquired this exact Session
// lease. Use the underlying store only through the typed Session recovery
// boundary; the generic startup pass continues to forbid all submissions.
// Keeping this hook here avoids changing the offline maintenance owner's flow.
func (p *offlineSessionPending) recoverOwnedStartup(ctx context.Context, resources *authorityResources) error {
	p.owner.stateMu.Lock()
	defer p.owner.stateMu.Unlock()
	if p.owner.closed || p.owner.sessionOwners[p.id] == nil ||
		p.owner.authority == nil || p.owner.authority.storeID != resources.storeID {
		return errors.New("offline pending recovery requires its bound Session lease")
	}
	sessions, err := sessionate.New(sessionate.Options{
		Process: resources.bound, Pending: p.authorityPendingStore,
		Space: authoritySpace, StoreID: resources.storeID,
	})
	if err != nil {
		return err
	}
	return sessions.RecoverSession(ctx, p.id)
}

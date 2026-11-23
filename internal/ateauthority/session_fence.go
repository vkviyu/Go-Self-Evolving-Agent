package ateauthority

import (
	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/model"
)

// RequireSessionSequence reads a native Session at this transaction's anchor.
// It also fences the final commit against all native publications, because a
// Session append changes the agent profile rather than the host state root.
// This uses the existing client lease and never opens a nested transaction.
func (tx *Tx) RequireSessionSequence(store model.StoreID, space model.SpaceID, id string, sequence uint64) error {
	if err := tx.check(true); err != nil {
		return err
	}
	if store != tx.view.Anchor.Fence.Store {
		return ErrIdentity
	}
	anchor := tx.view.Anchor
	anchor.Space = space
	result, err := tx.client.GetSession(tx.ctx, api.ReadSessionRequest{
		Space: space, Principal: tx.client.Identity().Principal, Anchor: &anchor, SessionID: id,
	})
	if err != nil {
		return mapReadError(err)
	}
	if result.Anchor.Fence != anchor.Fence || result.Anchor.Space != space {
		return ErrIdentity
	}
	if !result.Found || result.Session.ID != id || result.Session.Sequence != sequence {
		return ErrConflict
	}
	tx.fenceAll = true
	return nil
}

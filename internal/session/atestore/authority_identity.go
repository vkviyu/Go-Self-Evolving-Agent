package atestore

import "github.com/vkviyu/agent-temporal-engine/model"

// SessionAuthority identifies the native Session collection for cross-profile
// transactions. Session IDs are preserved verbatim in this collection.
func (s *Store) SessionAuthority() (model.StoreID, model.SpaceID) {
	return s.storeID, s.space
}

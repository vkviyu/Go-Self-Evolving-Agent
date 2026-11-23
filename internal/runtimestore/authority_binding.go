package runtimestore

import (
	"context"
	"fmt"
	"unicode/utf8"

	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
)

// BindAuthority makes the local checkpoint association independent of mutable
// filesystem paths. A replaced ATE store cannot inherit another store's runtime.
func (store *ATERequestStore) BindAuthority(ctx context.Context, id model.StoreID) error {
	if id == "" || len(id) > api.MaxIdentityBytes || !utf8.ValidString(string(id)) {
		return aterequest.ErrInvalid
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var foreign int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM (
        SELECT store_id FROM ate_pending_requests UNION ALL SELECT store_id FROM ate_rejected_requests
    ) WHERE store_id<>?`, string(id)).Scan(&foreign); err != nil {
		return err
	}
	if foreign != 0 {
		return fmt.Errorf("%w: saved requests belong to a different ATE store", aterequest.ErrConflict)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO runtime_authority(singleton, store_id) VALUES(1, ?) ON CONFLICT(singleton) DO NOTHING`, string(id)); err != nil {
		return err
	}
	var bound string
	if err := tx.QueryRowContext(ctx, `SELECT store_id FROM runtime_authority WHERE singleton=1`).Scan(&bound); err != nil {
		return err
	}
	if bound != string(id) {
		return fmt.Errorf("%w: runtime checkpoints belong to a different ATE store", aterequest.ErrConflict)
	}
	return tx.Commit()
}

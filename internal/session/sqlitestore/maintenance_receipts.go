package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/vkviyu/go-self-evolving-agent/internal/retention"
)

func loadMaintenanceReceipt(ctx context.Context, query rowQuery, id, operation string, result any) (bool, error) {
	var encoded []byte
	var storedOperation, sha string
	err := query.QueryRowContext(ctx, `SELECT operation,receipt,receipt_sha FROM maintenance_receipts WHERE plan_id=?`, id).Scan(&storedOperation, &encoded, &sha)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if storedOperation != operation || retention.Digest(encoded) != sha {
		return false, errors.New("maintenance receipt identity or checksum mismatch")
	}
	return true, json.Unmarshal(encoded, result)
}

// saveMaintenanceReceipt participates in the caller's mutation transaction.
func saveMaintenanceReceipt(ctx context.Context, tx *sql.Tx, id, operation string, receipt any) error {
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO maintenance_receipts(plan_id,operation,receipt,receipt_sha) VALUES(?,?,?,?)`, id, operation, encoded, retention.Digest(encoded))
	return err
}

package runtimestore

import (
	"context"
	"database/sql"
	"fmt"
)

// Query helpers read either the owner's DB or its current transaction. These
// ports omit Exec and transaction lifecycle methods; they express a dependency
// boundary, not a SQL authorization mechanism. In a write operation every helper
// must receive that operation's tx, never the pool: Database has one connection
// and transaction-local state must stay visible.
type rowQuery interface {
	// QueryRowContext issues a single-row lookup within the current database or
	// transaction.
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// A single decoder serves point lookups and list projections in column order.
type rowScanner interface {
	Scan(...any) error
}

// execCAS requires a conditional update to change exactly one row before its
// transaction may append events or update dependent projections. The earlier
// read is not proof that the UPDATE won: guards, triggers and future connection
// changes must not turn a lost compare-and-swap into a successful transition.
func execCAS(ctx context.Context, tx *sql.Tx, conflict error, statement string, args ...any) error {
	result, err := tx.ExecContext(ctx, statement, args...)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect conditional update: %w", err)
	}
	if count != 1 {
		return fmt.Errorf("%w: conditional update affected %d rows, expected one", conflict, count)
	}
	return nil
}

var (
	_ rowQuery   = (*sql.DB)(nil)
	_ rowQuery   = (*sql.Tx)(nil)
	_ rowScanner = (*sql.Row)(nil)
	_ rowScanner = (*sql.Rows)(nil)
)

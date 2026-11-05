package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

// Query helpers read either the owner's DB or its current transaction. These
// ports omit Exec and transaction lifecycle methods; they express a dependency
// boundary, not a SQL authorization mechanism. In a write operation every helper
// must receive that operation's tx, never the pool: Database has one connection
// and transaction-local state must stay visible.
type rowQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type readQuery interface {
	rowQuery
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
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

// recordError keeps the SQL driver's absence sentinel inside this adapter.
// Optional lookups must handle sql.ErrNoRows before calling this helper.
func recordError(err error, kind, id string) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s %s", orchestration.ErrNotFound, kind, id)
	}
	return err
}

var (
	_ readQuery  = (*sql.DB)(nil)
	_ readQuery  = (*sql.Tx)(nil)
	_ rowScanner = (*sql.Row)(nil)
	_ rowScanner = (*sql.Rows)(nil)
)

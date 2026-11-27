package runtimestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
)

const ateRequestsDDL = `
CREATE TABLE ate_pending_requests (
    space TEXT NOT NULL, principal TEXT NOT NULL, request_id TEXT NOT NULL,
    store_id TEXT NOT NULL,
    record BLOB NOT NULL CHECK(length(record) > 0 AND length(record) <= 12582912),
    checksum TEXT NOT NULL,
    attempted INTEGER NOT NULL DEFAULT 0 CHECK(attempted IN (0,1)),
    PRIMARY KEY(space, principal, request_id)
);
`

const ateRequestRejectionsDDL = `
CREATE TABLE ate_rejected_requests (
    space TEXT NOT NULL, principal TEXT NOT NULL, request_id TEXT NOT NULL,
    store_id TEXT NOT NULL,
    record BLOB NOT NULL CHECK(length(record) > 0 AND length(record) <= 65536),
    checksum TEXT NOT NULL,
    PRIMARY KEY(space, principal, request_id)
);
`

// ATERequestStore retains transport envelopes and body-free terminal receipts,
// never business projections. Immediate SQLite transactions serialize mutations
// even across independently opened Database handles.
type ATERequestStore struct{ db *sql.DB }

var (
	_ aterequest.PendingStore    = (*ATERequestStore)(nil)
	_ aterequest.RetirementStore = (*ATERequestStore)(nil)
)

// ATERequests returns the database-backed journal for pending ATE commands and their
// reconciliation.
func (database *Database) ATERequests() *ATERequestStore {
	return &ATERequestStore{db: database.db}
}

// requestKeyArgs returns the space, principal and request ID in the order used by journal
// queries.
func requestKeyArgs(key api.RequestKey) []any {
	return []any{key.Space, key.Principal, key.RequestID}
}

func validateRequestKey(key api.RequestKey) error {
	if err := key.Validate(); err != nil {
		return fmt.Errorf("%w: %w", aterequest.ErrInvalid, err)
	}
	return nil
}

// checkRequestAuthority checks the runtime's store binding before operating on a saved
// request.
func checkRequestAuthority(ctx context.Context, query rowQuery, id model.StoreID) error {
	var bound string
	err := query.QueryRowContext(ctx, `SELECT store_id FROM runtime_authority WHERE singleton=1`).Scan(&bound)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if bound != string(id) {
		return fmt.Errorf("%w: request belongs to another ATE authority", aterequest.ErrConflict)
	}
	return nil
}

// loadRequestTerminal reads and checks the persisted rejection for a request key and
// reports whether one exists.
func loadRequestTerminal(ctx context.Context, query rowQuery, key api.RequestKey) (aterequest.Rejection, bool, error) {
	var raw []byte
	var checksum string
	var id model.StoreID
	err := query.QueryRowContext(ctx, `SELECT store_id,record,checksum FROM ate_rejected_requests WHERE space=? AND principal=? AND request_id=?`, requestKeyArgs(key)...).Scan(&id, &raw, &checksum)
	if errors.Is(err, sql.ErrNoRows) {
		return aterequest.Rejection{}, false, nil
	}
	if err != nil {
		return aterequest.Rejection{}, false, err
	}
	terminal, err := decodeRequestTerminal(key, id, raw, checksum)
	return terminal, err == nil, err
}

func decodeRequestTerminal(key api.RequestKey, id model.StoreID, raw []byte, checksum string) (aterequest.Rejection, error) {
	terminal, err := aterequest.DecodeRejection(raw)
	if err != nil {
		return aterequest.Rejection{}, err
	}
	if sha256Hex(raw) != checksum || terminal.Key != key || terminal.StoreID != id {
		return aterequest.Rejection{}, fmt.Errorf("%w: terminal row identity or checksum", aterequest.ErrCorrupt)
	}
	return terminal, nil
}

const pendingSelect = `SELECT p.space,p.principal,p.request_id,p.store_id,p.record,p.checksum,p.attempted,
    t.store_id,t.record,t.checksum FROM ate_pending_requests p
    LEFT JOIN ate_rejected_requests t USING(space,principal,request_id)`

// Decode joined lifecycle metadata in the same read snapshot as the envelope.
func scanPendingRequest(row rowScanner) (aterequest.Record, int, error) {
	var key api.RequestKey
	var id model.StoreID
	var raw, terminalRaw []byte
	var checksum string
	var attempted int
	var terminalID, terminalChecksum sql.NullString
	if err := row.Scan(&key.Space, &key.Principal, &key.RequestID, &id, &raw, &checksum, &attempted,
		&terminalID, &terminalRaw, &terminalChecksum); err != nil {
		return aterequest.Record{}, 0, err
	}
	record, err := aterequest.DecodeRecord(raw)
	if err != nil {
		return aterequest.Record{}, 0, err
	}
	if sha256Hex(raw) != checksum || record.Request.Key() != key || record.StoreID != id || (attempted != 0 && attempted != 1) {
		return aterequest.Record{}, 0, fmt.Errorf("%w: pending row identity, checksum or attempt", aterequest.ErrCorrupt)
	}
	record.Attempted = attempted == 1
	if terminalID.Valid {
		terminal, err := decodeRequestTerminal(key, model.StoreID(terminalID.String), terminalRaw, terminalChecksum.String)
		if err != nil {
			return aterequest.Record{}, 0, err
		}
		if terminal.Retired || !record.Attempted || terminal.StoreID != id || terminal.Digest != record.Digest {
			return aterequest.Record{}, 0, fmt.Errorf("%w: terminal disagrees with pending request", aterequest.ErrCorrupt)
		}
		record.Rejection = &terminal.Result
	}
	return record, len(raw), nil
}

// loadPendingRequest retrieves a validated pending command identified by space, principal
// and request ID.
func loadPendingRequest(ctx context.Context, query rowQuery, key api.RequestKey) (aterequest.Record, bool, error) {
	record, _, err := scanPendingRequest(query.QueryRowContext(ctx, pendingSelect+` WHERE p.space=? AND p.principal=? AND p.request_id=?`, requestKeyArgs(key)...))
	if errors.Is(err, sql.ErrNoRows) {
		return aterequest.Record{}, false, nil
	}
	return record, err == nil, err
}

func (store *ATERequestStore) Get(ctx context.Context, key api.RequestKey) (aterequest.Record, bool, error) {
	if err := validateRequestKey(key); err != nil {
		return aterequest.Record{}, false, err
	}
	return loadPendingRequest(ctx, store.db, key)
}

func (store *ATERequestStore) GetTerminal(ctx context.Context, key api.RequestKey) (aterequest.Rejection, bool, error) {
	if err := validateRequestKey(key); err != nil {
		return aterequest.Rejection{}, false, err
	}
	return loadRequestTerminal(ctx, store.db, key)
}

func (store *ATERequestStore) List(ctx context.Context, options aterequest.ListOptions) ([]aterequest.Record, error) {
	if err := options.Validate(); err != nil {
		return nil, err
	}
	statement := pendingSelect
	var args []any
	if options.After != nil {
		statement += ` WHERE (p.space,p.principal,p.request_id) > (?,?,?)`
		args = requestKeyArgs(*options.After)
	}
	statement += ` ORDER BY p.space,p.principal,p.request_id LIMIT ?`
	args = append(args, options.Limit)
	rows, err := store.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []aterequest.Record
	total := 0
	for rows.Next() {
		record, size, err := scanPendingRequest(rows)
		if err != nil {
			return nil, err
		}
		if size > options.MaxBytes-total {
			if len(records) == 0 {
				return nil, aterequest.ErrLimit
			}
			break
		}
		records = append(records, record)
		total += size
	}
	return records, rows.Err()
}

func terminalMatches(terminal aterequest.Rejection, record aterequest.Record) bool {
	return terminal.StoreID == record.StoreID && terminal.Key == record.Request.Key() && terminal.Digest == record.Digest
}

func terminalInsertError(terminal aterequest.Rejection, record aterequest.Record) error {
	if !terminalMatches(terminal, record) {
		return aterequest.ErrConflict
	}
	if terminal.Retired {
		return aterequest.ErrRetired
	}
	return aterequest.ErrRejected
}

func (store *ATERequestStore) Insert(ctx context.Context, record aterequest.Record) error {
	raw, err := aterequest.EncodeRecord(record)
	if err != nil {
		return err
	}
	if len(raw) > aterequest.MaxRecordBytes {
		return aterequest.ErrLimit
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkRequestAuthority(ctx, tx, record.StoreID); err != nil {
		return err
	}
	terminal, found, err := loadRequestTerminal(ctx, tx, record.Request.Key())
	if err != nil {
		return err
	}
	if found {
		return terminalInsertError(terminal, record)
	}
	saved, found, err := loadPendingRequest(ctx, tx, record.Request.Key())
	if err != nil {
		return err
	}
	if found {
		if !aterequest.Equal(saved, record) {
			return aterequest.ErrConflict
		}
		return nil
	}
	// Lifecycle flags are store-owned; a caller cannot fabricate an attempt.
	if record.Attempted || record.Rejection != nil {
		return aterequest.ErrInvalid
	}
	// MaxListRecords/MaxListBytes bound each read, not the durable backlog.
	// Workload admission owns any complete-inventory/preparation budget; this
	// adapter must preserve outstanding commands beyond a single recovery page.
	args := append(requestKeyArgs(record.Request.Key()), record.StoreID, raw, sha256Hex(raw))
	if err := execCAS(ctx, tx, aterequest.ErrConflict, `INSERT INTO ate_pending_requests(space,principal,request_id,store_id,record,checksum) VALUES(?,?,?,?,?,?)`, args...); err != nil {
		return err
	}
	return tx.Commit()
}

func (store *ATERequestStore) BeginAttempt(ctx context.Context, record aterequest.Record) (bool, error) {
	if err := record.Validate(); err != nil {
		return false, err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := checkRequestAuthority(ctx, tx, record.StoreID); err != nil {
		return false, err
	}
	terminal, found, err := loadRequestTerminal(ctx, tx, record.Request.Key())
	if err != nil {
		return false, err
	}
	if found {
		return false, terminalInsertError(terminal, record)
	}
	saved, found, err := loadPendingRequest(ctx, tx, record.Request.Key())
	if err != nil {
		return false, err
	}
	if !found {
		return false, aterequest.ErrNotFound
	}
	if !aterequest.Equal(saved, record) {
		return false, aterequest.ErrConflict
	}
	if saved.Attempted {
		return false, nil
	}
	if err := execCAS(ctx, tx, aterequest.ErrConflict, `UPDATE ate_pending_requests SET attempted=1 WHERE space=? AND principal=? AND request_id=? AND attempted=0`, requestKeyArgs(record.Request.Key())...); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func deletePendingRequest(ctx context.Context, tx *sql.Tx, key api.RequestKey) error {
	return execCAS(ctx, tx, aterequest.ErrConflict, `DELETE FROM ate_pending_requests WHERE space=? AND principal=? AND request_id=?`, requestKeyArgs(key)...)
}

func (store *ATERequestStore) DeleteAcknowledged(ctx context.Context, record aterequest.Record, result api.CommitResult) error {
	if err := aterequest.VerifyAcknowledgement(record, result); err != nil {
		return err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkRequestAuthority(ctx, tx, record.StoreID); err != nil {
		return err
	}
	if _, found, err := loadRequestTerminal(ctx, tx, record.Request.Key()); err != nil {
		return err
	} else if found {
		return aterequest.ErrConflict
	}
	saved, found, err := loadPendingRequest(ctx, tx, record.Request.Key())
	if err != nil || !found {
		return err
	}
	if !aterequest.Equal(saved, record) {
		return aterequest.ErrConflict
	}
	if err := aterequest.VerifyAcknowledgement(saved, result); err != nil {
		return err
	}
	if err := deletePendingRequest(ctx, tx, record.Request.Key()); err != nil {
		return err
	}
	return tx.Commit()
}

func saveRequestTerminal(ctx context.Context, tx *sql.Tx, terminal aterequest.Rejection) error {
	raw, err := aterequest.EncodeRejection(terminal)
	if err != nil {
		return err
	}
	args := append(requestKeyArgs(terminal.Key), terminal.StoreID, raw, sha256Hex(raw))
	return execCAS(ctx, tx, aterequest.ErrConflict, `INSERT INTO ate_rejected_requests(space,principal,request_id,store_id,record,checksum) VALUES(?,?,?,?,?,?)`, args...)
}

func (store *ATERequestStore) DeleteRejected(ctx context.Context, record aterequest.Record, result api.CommitResult) error {
	if err := aterequest.VerifyRejection(record, result); err != nil {
		return err
	}
	// Commit the terminal marker separately: cleanup failure must never turn a
	// known rejection back into an unknown request that could be submitted.
	if err := store.persistRejection(ctx, record, result); err != nil {
		return err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	saved, found, err := loadPendingRequest(ctx, tx, record.Request.Key())
	if err != nil || !found {
		return err
	}
	if !aterequest.Equal(saved, record) || saved.Rejection == nil || *saved.Rejection != result {
		return aterequest.ErrConflict
	}
	if err := deletePendingRequest(ctx, tx, record.Request.Key()); err != nil {
		return err
	}
	return tx.Commit()
}

func (store *ATERequestStore) persistRejection(ctx context.Context, record aterequest.Record, result api.CommitResult) error {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkRequestAuthority(ctx, tx, record.StoreID); err != nil {
		return err
	}
	terminal, found, err := loadRequestTerminal(ctx, tx, record.Request.Key())
	if err != nil {
		return err
	}
	if found {
		if !terminalMatches(terminal, record) || terminal.Retired || terminal.Result != result {
			return aterequest.ErrConflict
		}
		return nil
	}
	saved, found, err := loadPendingRequest(ctx, tx, record.Request.Key())
	if err != nil {
		return err
	}
	if !found {
		return aterequest.ErrNotFound
	}
	if !aterequest.Equal(saved, record) || !saved.Attempted {
		return aterequest.ErrConflict
	}
	if err := saveRequestTerminal(ctx, tx, aterequest.Rejection{StoreID: record.StoreID, Key: record.Request.Key(), Digest: record.Digest, Result: result}); err != nil {
		return err
	}
	return tx.Commit()
}

func (store *ATERequestStore) RetireUnattempted(ctx context.Context, record aterequest.Record) error {
	if err := record.Validate(); err != nil {
		return err
	}
	if record.Attempted || record.Rejection != nil {
		return aterequest.ErrConflict
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkRequestAuthority(ctx, tx, record.StoreID); err != nil {
		return err
	}
	terminal, found, err := loadRequestTerminal(ctx, tx, record.Request.Key())
	if err != nil {
		return err
	}
	if found {
		if !terminalMatches(terminal, record) || !terminal.Retired {
			return aterequest.ErrConflict
		}
		return nil
	}
	saved, found, err := loadPendingRequest(ctx, tx, record.Request.Key())
	if err != nil {
		return err
	}
	if found && (!aterequest.Equal(saved, record) || saved.Attempted) {
		return aterequest.ErrConflict
	}
	if err := saveRequestTerminal(ctx, tx, aterequest.Rejection{StoreID: record.StoreID, Key: record.Request.Key(), Digest: record.Digest, Retired: true}); err != nil {
		return err
	}
	if found {
		if err := deletePendingRequest(ctx, tx, record.Request.Key()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

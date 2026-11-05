package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

var _ tokenusage.DurableReservationLedger = (*OrchestrationStore)(nil)

func (store *OrchestrationStore) ModelRequestSequence(ctx context.Context, teamID orchestration.TeamID, turnID string) (uint64, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT request_id FROM usage_reservations WHERE team_id = ? AND turn_id = ?`, string(teamID), turnID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var highest uint64
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		value, ok := strings.CutPrefix(id, turnID+":")
		if !ok {
			continue
		}
		sequence, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid reserved model request sequence: %w", err)
		}
		if sequence > highest {
			highest = sequence
		}
	}
	return highest, rows.Err()
}

// MarkDispatched is deliberately one-shot. A duplicate caller cannot send a
// second request under the same reservation, even if its payload is identical.
func (store *OrchestrationStore) MarkDispatched(ctx context.Context, id string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := store.db.ExecContext(ctx, `UPDATE usage_reservations SET dispatched_at = ?, updated_at = ? WHERE reservation_id = ? AND state = 'pending' AND lifecycle_version = 1 AND dispatched_at IS NULL AND outcome IS NULL`, now, now, id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("%w: reservation is missing, dispatched or closed", orchestration.ErrInvalidEvent)
	}
	return nil
}

func normalizeReceipt(record tokenusage.Record) (tokenusage.Record, error) {
	if err := record.TeamID.Validate(); err != nil {
		return record, err
	}
	if err := record.AgentID.Validate(); err != nil {
		return record, err
	}
	usage := record.Usage
	if record.RequestID == "" || usage.Input < 0 || usage.Output < 0 || usage.Total < 0 || usage.Cached < 0 || usage.Reasoning < 0 || record.CostMicros < 0 {
		return record, errors.New("invalid model usage receipt")
	}
	if record.RecordID == "" {
		record.RecordID = string(record.TeamID) + "_" + record.RequestID
	}
	if record.RecordedAt.IsZero() {
		record.RecordedAt = time.Now().UTC()
	}
	if record.Quality == "" {
		record.Quality = "provider"
	}
	return record, nil
}

func usageIdentityMatches(owner, record tokenusage.Record, includeModel bool) bool {
	return owner.TeamID == record.TeamID && owner.AgentID == record.AgentID && owner.RunID == record.RunID && owner.TurnID == record.TurnID && owner.RequestID == record.RequestID &&
		(!includeModel || owner.ModelProfile == record.ModelProfile && owner.Provider == record.Provider && owner.Model == record.Model)
}

func (store *OrchestrationStore) StageSettlement(ctx context.Context, id string, record tokenusage.Record) error {
	record, err := normalizeReceipt(record)
	if err != nil {
		return err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var owner tokenusage.Record
	var state string
	var dispatched sql.NullString
	var prior []byte
	err = tx.QueryRowContext(ctx, `SELECT team_id, agent_id, run_id, turn_id, request_id, model_profile, provider, model, state, dispatched_at, outcome FROM usage_reservations WHERE reservation_id = ?`, id).Scan(&owner.TeamID, &owner.AgentID, &owner.RunID, &owner.TurnID, &owner.RequestID, &owner.ModelProfile, &owner.Provider, &owner.Model, &state, &dispatched, &prior)
	if err != nil {
		return recordError(err, "usage reservation", id)
	}
	if !usageIdentityMatches(owner, record, true) {
		return fmt.Errorf("%w: receipt identity mismatch", orchestration.ErrInvalidEvent)
	}
	if state == string(tokenusage.ReservationReleased) {
		return errors.New("cannot stage a receipt for a released reservation")
	}
	if len(prior) > 0 {
		var existing tokenusage.Record
		if err := json.Unmarshal(prior, &existing); err != nil {
			return err
		}
		if !usageRecordMatches(existing, record) {
			return fmt.Errorf("%w: different receipt", orchestration.ErrIdempotencyConflict)
		}
		return tx.Commit()
	}
	if state != string(tokenusage.ReservationPending) || !dispatched.Valid {
		return errors.New("cannot stage a receipt before dispatch or after closure")
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE usage_reservations SET outcome = ?, updated_at = ? WHERE reservation_id = ?`, encoded, time.Now().UTC().Format(time.RFC3339Nano), id); err != nil {
		return err
	}
	return tx.Commit()
}

// RecoverReservations is an exclusive-startup operation, never a periodic
// reaper. New un-dispatched requests can be refunded; legacy and dispatched
// unknown outcomes must retain their conservative reserved consumption.
func (store *OrchestrationStore) RecoverReservations(ctx context.Context) (int, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT reservation_id, team_id, agent_id, run_id, turn_id, request_id, model_profile, provider, model, input_tokens, output_tokens, total_tokens, cost_micros, lifecycle_version, dispatched_at, outcome FROM usage_reservations WHERE state = 'pending' ORDER BY reservation_id`)
	if err != nil {
		return 0, err
	}
	type pending struct {
		id         string
		record     tokenusage.Record
		version    int
		dispatched sql.NullString
		receipt    []byte
	}
	var requests []pending
	for rows.Next() {
		var request pending
		r := &request.record
		if err := rows.Scan(&request.id, &r.TeamID, &r.AgentID, &r.RunID, &r.TurnID, &r.RequestID, &r.ModelProfile, &r.Provider, &r.Model, &r.Usage.Input, &r.Usage.Output, &r.Usage.Total, &r.CostMicros, &request.version, &request.dispatched, &request.receipt); err != nil {
			rows.Close()
			return 0, err
		}
		requests = append(requests, request)
	}
	err = rows.Err()
	if closeErr := rows.Close(); err != nil || closeErr != nil {
		return 0, errors.Join(err, closeErr)
	}
	for _, request := range requests {
		if request.version != 0 && request.version != 1 {
			return 0, fmt.Errorf("unsupported reservation lifecycle version: %d", request.version)
		}
		r := request.record
		state := tokenusage.ReservationSettled
		r.Quality = "estimated_recovery"
		existing, lookupErr := existingUsageRecord(ctx, tx, r.TeamID, r.RequestID)
		switch {
		case lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows):
			return 0, lookupErr
		case lookupErr == nil:
			if !usageIdentityMatches(r, existing, request.version == 1) {
				return 0, errors.New("recovery usage owner mismatch")
			}
			r = existing
		case len(request.receipt) > 0:
			if err := json.Unmarshal(request.receipt, &r); err != nil {
				return 0, err
			}
			if !usageIdentityMatches(request.record, r, true) {
				return 0, errors.New("recovery receipt identity mismatch")
			}
		case request.version == 1 && !request.dispatched.Valid:
			state = tokenusage.ReservationReleased
		}
		if state == tokenusage.ReservationSettled {
			r, err = normalizeReceipt(r)
			if err != nil {
				return 0, err
			}
			if len(request.receipt) > 0 && lookupErr == nil {
				var receipt tokenusage.Record
				if err := json.Unmarshal(request.receipt, &receipt); err != nil {
					return 0, err
				}
				if !usageRecordMatches(receipt, r) {
					return 0, errors.New("recovery receipt conflicts with ledger")
				}
			}
			if err := recordUsageTx(ctx, tx, r); err != nil {
				return 0, err
			}
		}
		if err := execCAS(ctx, tx, orchestration.ErrSequenceConflict, `UPDATE usage_reservations SET state = ?, updated_at = ? WHERE reservation_id = ? AND state = 'pending'`, string(state), time.Now().UTC().Format(time.RFC3339Nano), request.id); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(requests), nil
}

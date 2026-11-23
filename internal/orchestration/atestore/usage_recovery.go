package atestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

func (store *Store) ModelRequestSequence(ctx context.Context, id orchestration.TeamID, turnID string) (highest uint64, err error) {
	err = store.authority.View(usageContext(ctx), func(tx *ateauthority.Tx) error {
		reservations, err := listRecords[usageReservation](tx, "usage-reservation")
		if err != nil {
			return err
		}
		for _, reservation := range reservations {
			if reservation.TeamID != id || reservation.TurnID != turnID {
				continue
			}
			value, ok := strings.CutPrefix(reservation.RequestID, turnID+":")
			if !ok {
				continue
			}
			sequence, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return fmt.Errorf("invalid reserved model request sequence: %w", err)
			}
			highest = max(highest, sequence)
		}
		return nil
	})
	return
}

// MarkDispatched is one-shot, including retries with identical inputs.
func (store *Store) MarkDispatched(ctx context.Context, id string) error {
	return store.authority.Update(usageContext(ctx), func(tx *ateauthority.Tx) error {
		stored, err := getRecord[usageReservation](tx, "usage-reservation", id)
		if errors.Is(err, orchestration.ErrNotFound) {
			return fmt.Errorf("%w: reservation is missing, dispatched or closed", orchestration.ErrInvalidEvent)
		}
		if err != nil {
			return err
		}
		if stored.State != tokenusage.ReservationPending || stored.LifecycleVersion != 1 || stored.Dispatched || len(stored.Outcome) != 0 {
			return fmt.Errorf("%w: reservation is missing, dispatched or closed", orchestration.ErrInvalidEvent)
		}
		stored.Dispatched, stored.UpdatedAt = true, time.Now().UTC()
		return putRecord(tx, "usage-reservation", id, stored)
	})
}

func usageOwner(reservation usageReservation) tokenusage.Record {
	return tokenusage.Record{TeamID: reservation.TeamID, AgentID: reservation.AgentID, RunID: reservation.RunID,
		TurnID: reservation.TurnID, RequestID: reservation.RequestID, ModelProfile: reservation.ModelProfile,
		Provider: reservation.Provider, Model: reservation.Model,
		Usage:      tokenusage.Snapshot{Input: reservation.InputTokens, Output: reservation.OutputTokens, Total: reservation.TotalTokens},
		CostMicros: reservation.CostMicros}
}

func usageIdentityMatches(owner, record tokenusage.Record, includeModel bool) bool {
	return owner.TeamID == record.TeamID && owner.AgentID == record.AgentID && owner.RunID == record.RunID &&
		owner.TurnID == record.TurnID && owner.RequestID == record.RequestID &&
		(!includeModel || owner.ModelProfile == record.ModelProfile && owner.Provider == record.Provider && owner.Model == record.Model)
}

func (store *Store) StageSettlement(ctx context.Context, id string, record tokenusage.Record) error {
	record, err := usageNormalizeReceipt(record)
	if err != nil {
		return err
	}
	return store.authority.Update(usageContext(ctx), func(tx *ateauthority.Tx) error {
		stored, err := getRecord[usageReservation](tx, "usage-reservation", id)
		if err != nil {
			return err
		}
		if !usageIdentityMatches(usageOwner(stored), record, true) {
			return fmt.Errorf("%w: receipt identity mismatch", orchestration.ErrInvalidEvent)
		}
		if stored.State == tokenusage.ReservationReleased {
			return errors.New("cannot stage a receipt for a released reservation")
		}
		if len(stored.Outcome) > 0 {
			var existing tokenusage.Record
			if err := json.Unmarshal(stored.Outcome, &existing); err != nil {
				return err
			}
			if !usageRecordMatches(existing, record) {
				return fmt.Errorf("%w: different receipt", orchestration.ErrIdempotencyConflict)
			}
			return nil
		}
		if stored.State != tokenusage.ReservationPending || !stored.Dispatched {
			return errors.New("cannot stage a receipt before dispatch or after closure")
		}
		stored.Outcome, err = json.Marshal(record)
		if err != nil {
			return err
		}
		stored.UpdatedAt = time.Now().UTC()
		return putRecord(tx, "usage-reservation", id, stored)
	})
}

// RecoverReservations requires exclusive startup ownership, never concurrent
// model requests. All repairs commit together; failures leave every reservation
// untouched. Dispatched or legacy unknown outcomes consume their full allowance.
func (store *Store) RecoverReservations(ctx context.Context) (int, error) {
	return store.usageRecoverReservations(ctx, "")
}

// RecoverTeamReservations excludes requests owned by other live runtimes.
func (store *Store) RecoverTeamReservations(ctx context.Context, team orchestration.TeamID) (int, error) {
	if err := team.Validate(); err != nil {
		return 0, err
	}
	return store.usageRecoverReservations(ctx, team)
}

// usageRecoverReservations applies the same recovery rules within the chosen scope.
func (store *Store) usageRecoverReservations(ctx context.Context, team orchestration.TeamID) (int, error) {
	count := 0
	err := store.authority.Update(usageContext(ctx), func(tx *ateauthority.Tx) error {
		count = 0 // Update may retry a proven precondition conflict.
		reservations, err := listRecords[usageReservation](tx, "usage-reservation")
		if err != nil {
			return err
		}
		slices.SortFunc(reservations, func(a, b usageReservation) int { return strings.Compare(a.ID, b.ID) })
		for _, reservation := range reservations {
			if team != "" && reservation.TeamID != team {
				continue
			}
			if reservation.State != tokenusage.ReservationPending {
				continue
			}
			if reservation.LifecycleVersion != 0 && reservation.LifecycleVersion != 1 {
				return fmt.Errorf("unsupported reservation lifecycle version: %d", reservation.LifecycleVersion)
			}
			owner := usageOwner(reservation)
			record := owner
			record.Quality = "estimated_recovery"
			state := tokenusage.ReservationSettled
			existing, lookupErr := usageExistingRecord(tx, owner.TeamID, owner.RequestID)
			switch {
			case lookupErr != nil && !errors.Is(lookupErr, orchestration.ErrNotFound):
				return lookupErr
			case lookupErr == nil:
				if !usageIdentityMatches(owner, existing, reservation.LifecycleVersion == 1) {
					return errors.New("recovery usage owner mismatch")
				}
				record = existing
			case len(reservation.Outcome) > 0:
				if err := json.Unmarshal(reservation.Outcome, &record); err != nil {
					return err
				}
				if !usageIdentityMatches(owner, record, true) {
					return errors.New("recovery receipt identity mismatch")
				}
			case reservation.LifecycleVersion == 1 && !reservation.Dispatched:
				state = tokenusage.ReservationReleased
			}
			if state == tokenusage.ReservationSettled {
				record, err = usageNormalizeReceipt(record)
				if err != nil {
					return err
				}
				if len(reservation.Outcome) > 0 && lookupErr == nil {
					var receipt tokenusage.Record
					if err := json.Unmarshal(reservation.Outcome, &receipt); err != nil {
						return err
					}
					if !usageRecordMatches(receipt, record) {
						return errors.New("recovery receipt conflicts with ledger")
					}
				}
				if err := usageRecordTx(tx, record); err != nil {
					return err
				}
			}
			reservation.State, reservation.UpdatedAt = state, time.Now().UTC()
			if err := putRecord(tx, "usage-reservation", reservation.ID, reservation); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

package atestore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/retention"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

type retentionRecord struct {
	Receipt  retention.Receipt
	Events   []session.PendingEvent
	Complete bool
}

// CloseRetention commits a prepared receipt and admission tombstones first,
// then idempotent native session audit events, then a completed native receipt.
// Only a successful return authorizes local resource cleanup. It is not an
// atomic transaction across ATE session records, authority records and SQLite.
func (store *Store) CloseRetention(ctx context.Context, id session.ID, planID string, confirmed bool, local []retention.Consumer) (retention.Receipt, error) {
	if !confirmed || id == "" || planID == "" {
		return retention.Receipt{}, errors.New("retention requires session, reviewed plan ID and explicit confirmation")
	}
	var record retentionRecord
	var found bool
	err := store.authority.View(ctx, func(tx *ateauthority.Tx) (err error) {
		found, err = tx.Get(recordKey("retention-receipt", planID), &record)
		return err
	})
	if err != nil {
		return retention.Receipt{}, err
	}
	if found {
		if record.Receipt.Plan.SessionID != id || record.Receipt.Plan.ID != planID {
			return retention.Receipt{}, retention.ErrPlanChanged
		}
	} else {
		turns, sequence, err := store.retentionTurns(ctx, id)
		if err != nil {
			return retention.Receipt{}, err
		}
		err = store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
			plan, err := planNativeRetention(tx, id, sequence, turns, local)
			if err != nil {
				return err
			}
			if plan.ID != planID {
				return retention.ErrPlanChanged
			}
			if len(plan.Blockers) > 0 {
				return fmt.Errorf("%w: %v", retention.ErrBlocked, plan.Blockers)
			}
			if len(plan.Consumers) == 0 {
				return errors.New("retention plan has no consumers to close")
			}
			record = retentionRecord{Receipt: retention.Receipt{Plan: plan, ClosedAt: time.Now().UTC()}}
			for _, c := range plan.Consumers {
				if err := createRecord(tx, "retention-consumer", retentionReferenceKey(c.Reference), planID); err != nil {
					return err
				}
				if c.Reference.Kind == "turn" {
					record.Events = append(record.Events, session.PendingEvent{
						Type: session.EventTurnArchived, CorrelationID: c.Reference.ID, CausationID: planID, RuntimeSnapshotID: c.SnapshotID,
						Payload: session.MustJSONPayload(map[string]any{"plan_id": planID, "history_preserved": true, "effects_reverted": false}),
					})
				}
			}
			return createRecord(tx, "retention-receipt", planID, record)
		})
		if err != nil {
			return retention.Receipt{}, err
		}
	}
	if err := store.finishRetention(ctx, record); err != nil {
		return retention.Receipt{}, err
	}
	return record.Receipt, nil
}

func (store *Store) finishRetention(ctx context.Context, record retentionRecord) error {
	if record.Complete {
		return nil
	}
	if store.sessions == nil {
		return errors.New("authoritative session events unavailable")
	}
	for i, pending := range record.Events {
		sequence := record.Receipt.Plan.Sequence + uint64(i)
		events, err := store.sessions.Load(ctx, record.Receipt.Plan.SessionID, sequence, 1)
		if err != nil {
			return err
		}
		if len(events) > 0 {
			e := events[0]
			if e.Sequence != sequence+1 || e.Type != pending.Type || e.CorrelationID != pending.CorrelationID ||
				e.CausationID != pending.CausationID || e.RuntimeSnapshotID != pending.RuntimeSnapshotID || !bytes.Equal(e.Payload, pending.Payload) {
				return fmt.Errorf("%w: retention audit diverged", session.ErrSequenceConflict)
			}
		} else if _, err := store.sessions.Append(ctx, record.Receipt.Plan.SessionID, sequence, pending); err != nil {
			return err
		}
	}
	return store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		current, err := getRecord[retentionRecord](tx, "retention-receipt", record.Receipt.Plan.ID)
		if err != nil || current.Complete {
			return err
		}
		current.Complete = true
		return putRecord(tx, "retention-receipt", current.Receipt.Plan.ID, current)
	})
}

// ResumeRetentions advances only the owned Session's prepared native receipts.
// The host must reconcile pending native requests first. Runtime cleanup remains
// explicit and retryable through CloseRetention with the original plan ID.
func (store *Store) ResumeRetentions(ctx context.Context, id session.ID) error {
	if id == "" {
		return errors.New("retention recovery requires an owned session")
	}
	var records []retentionRecord
	if err := store.authority.View(ctx, func(tx *ateauthority.Tx) (err error) {
		records, err = listRecords[retentionRecord](tx, "retention-receipt")
		return err
	}); err != nil {
		return err
	}
	for _, record := range records {
		if record.Receipt.Plan.SessionID == id && !record.Complete {
			if err := store.finishRetention(ctx, record); err != nil {
				return err
			}
		}
	}
	return nil
}

// CheckRetentionReference must run before every snapshot admission (including
// Flow references), composed with CheckSnapshotReference. No runtime database
// can override these shared ATE tombstones, including prepared closures.
func (store *Store) CheckRetentionReference(ctx context.Context, ref tool.SnapshotReference) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	return store.authority.View(ctx, func(tx *ateauthority.Tx) error {
		var receiptID string
		found, err := tx.Get(recordKey("retention-consumer", retentionReferenceKey(ref)), &receiptID)
		if err != nil {
			return err
		}
		if found {
			return errors.Join(tool.ErrSnapshotReferenceReleased, session.ErrRecoveryArchived)
		}
		return nil
	})
}

// RetentionReceipt returns only a completed native receipt; it is useful for
// retrying cleanup in another runtime database without inferring global
// lifecycle from that database's local reference rows.
func (store *Store) RetentionReceipt(ctx context.Context, id session.ID, planID string) (retention.Receipt, error) {
	var record retentionRecord
	err := store.authority.View(ctx, func(tx *ateauthority.Tx) (err error) {
		record, err = getRecord[retentionRecord](tx, "retention-receipt", planID)
		return err
	})
	if err != nil {
		return retention.Receipt{}, err
	}
	if !record.Complete || record.Receipt.Plan.SessionID != id {
		return retention.Receipt{}, orchestration.ErrSequenceConflict
	}
	return record.Receipt, nil
}

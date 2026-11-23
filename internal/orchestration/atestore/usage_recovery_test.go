package atestore

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

func TestUsageRecoveryPreservesUnknownConsumptionAndReceipts(t *testing.T) {
	for _, scenario := range []struct {
		name, quality                       string
		dispatch, receipt, legacy, recorded bool
		tokens                              int
		cost                                int64
	}{
		{name: "known-unsent"},
		{name: "dispatched-unknown", dispatch: true, tokens: 40, cost: 50, quality: "estimated_recovery"},
		{name: "provider-receipt", dispatch: true, receipt: true, tokens: 7, cost: 9, quality: "provider"},
		{name: "legacy-unknown", legacy: true, tokens: 40, cost: 50, quality: "estimated_recovery"},
		{name: "already-recorded", dispatch: true, recorded: true, tokens: 7, cost: 9, quality: "provider"},
		{name: "legacy-recorded", legacy: true, recorded: true, tokens: 7, cost: 9, quality: "provider"},
		{name: "recorded-and-receipt", dispatch: true, receipt: true, recorded: true, tokens: 7, cost: 9, quality: "provider"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := context.Background()
			store := newTestStore(t)
			reservation, record := usageTestReservation(t, store)
			if scenario.dispatch {
				if err := store.MarkDispatched(ctx, reservation.ID); err != nil {
					t.Fatal(err)
				}
			}
			if scenario.receipt {
				if err := store.StageSettlement(ctx, reservation.ID, record); err != nil {
					t.Fatal(err)
				}
			}
			if scenario.legacy {
				if err := store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
					value, err := getRecord[usageReservation](tx, "usage-reservation", reservation.ID)
					if err != nil {
						return err
					}
					value.LifecycleVersion, value.ModelProfile, value.Provider, value.Model = 0, "", "", ""
					return putRecord(tx, "usage-reservation", value.ID, value)
				}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario.recorded {
				if err := store.RecordUsage(ctx, record); err != nil {
					t.Fatal(err)
				}
			}
			store = &Store{authority: store.authority, sessions: store.sessions}
			if count, err := store.RecoverReservations(ctx); err != nil || count != 1 {
				t.Fatalf("recover: %d %v", count, err)
			}
			if count, err := store.RecoverReservations(ctx); err != nil || count != 0 {
				t.Fatalf("repeat recovery: %d %v", count, err)
			}
			budget, err := store.Budget(ctx, reservation.TeamID)
			if err != nil || budget.ReservedTotalTokens != 0 || budget.TotalTokens != scenario.tokens || budget.CostMicros != scenario.cost {
				t.Fatalf("recovered budget: %+v %v", budget, err)
			}
			if err := store.authority.View(ctx, func(tx *ateauthority.Tx) error {
				value, err := getRecord[usageReservation](tx, "usage-reservation", reservation.ID)
				if err != nil {
					return err
				}
				want := tokenusage.ReservationSettled
				if scenario.tokens == 0 {
					want = tokenusage.ReservationReleased
				}
				if value.State != want {
					t.Errorf("state: %s, want %s", value.State, want)
				}
				ledger, err := usageExistingRecord(tx, record.TeamID, record.RequestID)
				if scenario.tokens == 0 {
					if !errors.Is(err, orchestration.ErrNotFound) {
						t.Errorf("unsent request charged: %+v %v", ledger, err)
					}
					return nil
				}
				if err != nil {
					return err
				}
				if ledger.Quality != scenario.quality {
					t.Errorf("quality: %s", ledger.Quality)
				}
				if scenario.quality == "provider" && ledger.Usage != record.Usage {
					t.Errorf("lost receipt detail: %+v", ledger)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if sequence, err := store.ModelRequestSequence(ctx, reservation.TeamID, "turn"); err != nil || sequence != 7 {
				t.Fatalf("closed request identity lost: %d %v", sequence, err)
			}
		})
	}
}

func TestUsageRecoveryCorruptionRollsBackEarlierRepairs(t *testing.T) {
	for _, scenario := range []string{"receipt-json", "receipt-owner", "ledger-owner", "ledger-model", "receipt-conflict", "future-version", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			store := newTestStore(t)
			earlier, earlierRecord := usageTestReservation(t, store)
			later, laterRecord := usageTestReservation(t, store)
			if earlier.ID > later.ID {
				earlier, later = later, earlier
				earlierRecord, laterRecord = laterRecord, earlierRecord
			}
			if err := store.MarkDispatched(ctx, later.ID); err != nil {
				t.Fatal(err)
			}
			if err := store.StageSettlement(ctx, later.ID, laterRecord); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "ledger-owner", "ledger-model", "receipt-conflict":
				conflict := laterRecord
				switch scenario {
				case "ledger-owner":
					conflict.TurnID = "different"
				case "ledger-model":
					conflict.Model = "different"
				case "receipt-conflict":
					conflict.Usage.Total++
				}
				if err := store.RecordUsage(ctx, conflict); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			default:
				if err := store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
					value, err := getRecord[usageReservation](tx, "usage-reservation", later.ID)
					if err != nil {
						return err
					}
					switch scenario {
					case "receipt-json":
						value.Outcome = json.RawMessage(`"not a receipt"`)
					case "receipt-owner":
						value.TurnID = "different"
					case "future-version":
						value.LifecycleVersion = 2
					}
					return putRecord(tx, "usage-reservation", value.ID, value)
				}); err != nil {
					t.Fatal(err)
				}
			}
			if count, err := store.RecoverReservations(ctx); err == nil || count != 0 {
				t.Fatalf("corrupt recovery: %d %v", count, err)
			}
			if err := store.authority.View(context.Background(), func(tx *ateauthority.Tx) error {
				for _, id := range []string{earlier.ID, later.ID} {
					value, err := getRecord[usageReservation](tx, "usage-reservation", id)
					if err != nil {
						return err
					}
					if value.State != tokenusage.ReservationPending {
						t.Errorf("partial recovery committed: %s %s", id, value.State)
					}
				}
				if _, err := usageExistingRecord(tx, earlierRecord.TeamID, earlierRecord.RequestID); !errors.Is(err, orchestration.ErrNotFound) {
					t.Errorf("partial ledger write: %v", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUsageSettlementRecordConflictLeavesReservationPending(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	reservation, receipt := usageTestReservation(t, store)
	other := receipt
	other.RequestID, other.RecordID = "other", "occupied"
	if err := store.RecordUsage(ctx, other); err != nil {
		t.Fatal(err)
	}
	receipt.RecordID = other.RecordID
	if err := store.SettleReservation(ctx, reservation.ID, receipt); err == nil {
		t.Fatal("duplicate ledger primary key accepted")
	}
	budget, err := store.Budget(ctx, reservation.TeamID)
	if err != nil || budget.ReservedTotalTokens != 40 || budget.TotalTokens != 7 {
		t.Fatalf("partial settlement: %+v %v", budget, err)
	}
}

func TestUsageSequenceRejectsMalformedSuffixAndTotalsDoNotWrap(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	team, agent := usageTestOwner(t, store)
	if _, err := store.Reserve(ctx, tokenusage.ReservationRequest{TeamID: team, AgentID: agent, TurnID: "turn", RequestID: "turn:invalid"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ModelRequestSequence(ctx, team, "turn"); err == nil {
		t.Fatal("malformed model sequence ignored")
	}
	if _, err := store.Reserve(ctx, tokenusage.ReservationRequest{TeamID: team, AgentID: agent, RequestID: "overflow", InputTokens: math.MaxInt, OutputTokens: 1}); err == nil {
		t.Fatal("reservation total wrapped")
	}
	if err := store.RecordUsage(ctx, tokenusage.Record{TeamID: team, AgentID: agent, RequestID: "large", Usage: tokenusage.Snapshot{Total: math.MaxInt}}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordUsage(ctx, tokenusage.Record{TeamID: team, AgentID: agent, RequestID: "extra", Usage: tokenusage.Snapshot{Total: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Budget(ctx, team); err == nil {
		t.Fatal("budget aggregate wrapped")
	}
	if _, err := store.UsageReport(ctx, tokenusage.UsageQuery{TeamID: team}); err == nil {
		t.Fatal("report aggregate wrapped")
	}
}

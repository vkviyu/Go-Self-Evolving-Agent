package sqlitestore

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

func recoveryReservation(t *testing.T, database *Database, requestID string) (tokenusage.Reservation, tokenusage.Record) {
	t.Helper()
	ctx := context.Background()
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "usage-recovery"})
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := store.Reserve(ctx, tokenusage.ReservationRequest{
		TeamID: root.TeamID, AgentID: root.AgentID, TurnID: "turn", RequestID: requestID,
		ModelProfile: "primary", Provider: "test", Model: "test-model",
		InputTokens: 30, OutputTokens: 10, TotalTokens: 40, CostMicros: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	return reservation, tokenusage.Record{
		TeamID: root.TeamID, AgentID: root.AgentID, TurnID: "turn", RequestID: requestID,
		ModelProfile: "primary", Provider: "test", Model: "test-model",
		Usage: tokenusage.Snapshot{Input: 5, Cached: 2, Output: 2, Reasoning: 1, Total: 7}, CostMicros: 9,
	}
}

func openUsageRecoveryDB(t *testing.T, path string) *Database {
	t.Helper()
	database, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func TestUsageRecoveryReopensWithoutRefundingUnknownConsumption(t *testing.T) {
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
			path := filepath.Join(t.TempDir(), "usage.db")
			database := openUsageRecoveryDB(t, path)
			reservation, record := recoveryReservation(t, database, "turn:7")
			store := database.Orchestration()
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
				if _, err := database.db.Exec(`UPDATE usage_reservations SET lifecycle_version = 0, model_profile = '', provider = '', model = '' WHERE reservation_id = ?`, reservation.ID); err != nil {
					t.Fatal(err)
				}
			}
			if scenario.recorded {
				if err := store.RecordUsage(ctx, record); err != nil {
					t.Fatal(err)
				}
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}
			database = openUsageRecoveryDB(t, path)
			store = database.Orchestration()
			if count, err := store.RecoverReservations(ctx); err != nil || count != 1 {
				t.Fatalf("recover=%d %v", count, err)
			}
			if count, err := store.RecoverReservations(ctx); err != nil || count != 0 {
				t.Fatalf("repeat recovery=%d %v", count, err)
			}
			budget, err := store.Budget(ctx, reservation.TeamID)
			if err != nil {
				t.Fatal(err)
			}
			if budget.ReservedTotalTokens != 0 || budget.TotalTokens != scenario.tokens || budget.CostMicros != scenario.cost {
				t.Fatalf("recovered budget=%#v", budget)
			}
			var state string
			if err := database.db.QueryRow(`SELECT state FROM usage_reservations WHERE reservation_id = ?`, reservation.ID).Scan(&state); err != nil {
				t.Fatal(err)
			}
			wantState := tokenusage.ReservationSettled
			if scenario.tokens == 0 {
				wantState = tokenusage.ReservationReleased
			}
			if state != string(wantState) {
				t.Fatalf("state=%s", state)
			}
			report, err := store.UsageReport(ctx, tokenusage.UsageQuery{TeamID: reservation.TeamID})
			if err != nil {
				t.Fatal(err)
			}
			if scenario.tokens == 0 {
				if report.Aggregate.Records != 0 {
					t.Fatal("unsent request was charged")
				}
			} else {
				if report.Aggregate.Records != 1 {
					t.Fatalf("duplicate charge=%#v", report)
				}
				recovered, err := existingUsageRecord(ctx, database.db, reservation.TeamID, reservation.RequestID)
				if err != nil {
					t.Fatal(err)
				}
				if recovered.Quality != scenario.quality {
					t.Fatalf("quality=%s", recovered.Quality)
				}
				if scenario.quality == "provider" && recovered.Usage != record.Usage {
					t.Fatalf("lost receipt=%#v", recovered)
				}
			}
			// Even a refunded request keeps its durable identity; a crash before
			// model.requested must not cause the process-local counter to reuse it.
			if sequence, err := store.ModelRequestSequence(ctx, reservation.TeamID, "turn"); err != nil || sequence != 7 {
				t.Fatalf("sequence=%d %v", sequence, err)
			}
		})
	}
}

func TestUsageDispatchAndReceiptCannotBeReusedOrRefunded(t *testing.T) {
	ctx := context.Background()
	database := openUsageRecoveryDB(t, ":memory:")
	reservation, record := recoveryReservation(t, database, "turn:1")
	store := database.Orchestration()
	if err := store.StageSettlement(ctx, reservation.ID, record); err == nil {
		t.Fatal("receipt accepted before dispatch")
	}
	if err := store.MarkDispatched(ctx, reservation.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDispatched(ctx, reservation.ID); err == nil {
		t.Fatal("dispatch repeated")
	}
	if err := store.ReleaseReservation(ctx, reservation.ID, "failed send"); err == nil {
		t.Fatal("unknown consumption refunded")
	}
	wrong := record
	wrong.Model = "another-model"
	if err := store.StageSettlement(ctx, reservation.ID, wrong); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("wrong model=%v", err)
	}
	if err := store.StageSettlement(ctx, reservation.ID, record); err != nil {
		t.Fatal(err)
	}
	if err := store.StageSettlement(ctx, reservation.ID, record); err != nil {
		t.Fatal(err)
	}
	wrong = record
	wrong.Usage.Output++
	if err := store.StageSettlement(ctx, reservation.ID, wrong); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("overwrite=%v", err)
	}
	if err := store.SettleReservation(ctx, reservation.ID, wrong); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("settlement overwrite=%v", err)
	}
	if err := store.ReleaseReservation(ctx, reservation.ID, "receipt exists"); err == nil {
		t.Fatal("known receipt refunded")
	}
	if err := store.SettleReservation(ctx, reservation.ID, record); err != nil {
		t.Fatal(err)
	}
	if err := store.StageSettlement(ctx, reservation.ID, record); err != nil {
		t.Fatal(err)
	}
}

func TestUsageRecoveryRejectsCorruptionAtomically(t *testing.T) {
	for _, scenario := range []string{"receipt-json", "receipt-owner", "ledger-owner", "ledger-model", "receipt-conflict", "future-version", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			database := openUsageRecoveryDB(t, ":memory:")
			store := database.Orchestration()
			reservation, record := recoveryReservation(t, database, "turn:1")
			other, _ := recoveryReservation(t, database, "turn:2")
			if err := store.MarkDispatched(ctx, reservation.ID); err != nil {
				t.Fatal(err)
			}
			if err := store.StageSettlement(ctx, reservation.ID, record); err != nil {
				t.Fatal(err)
			}
			var err error
			switch scenario {
			case "receipt-json":
				_, err = database.db.Exec(`UPDATE usage_reservations SET outcome = '{' WHERE reservation_id = ?`, reservation.ID)
			case "receipt-owner":
				_, err = database.db.Exec(`UPDATE usage_reservations SET turn_id = 'different-turn' WHERE reservation_id = ?`, reservation.ID)
			case "ledger-owner", "ledger-model":
				if scenario == "ledger-owner" {
					record.TurnID = "different-turn"
				} else {
					record.Model = "different-model"
				}
				err = store.RecordUsage(ctx, record)
			case "receipt-conflict":
				record.Usage.Total++
				err = store.RecordUsage(ctx, record)
			case "future-version":
				_, err = database.db.Exec(`UPDATE usage_reservations SET lifecycle_version = 2 WHERE reservation_id = ?`, reservation.ID)
			case "canceled":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			if err != nil {
				t.Fatal(err)
			}
			if count, err := store.RecoverReservations(ctx); err == nil || count != 0 {
				t.Fatalf("corrupt recovery=%d %v", count, err)
			}
			for _, id := range []string{reservation.ID, other.ID} {
				var state string
				if err := database.db.QueryRow(`SELECT state FROM usage_reservations WHERE reservation_id = ?`, id).Scan(&state); err != nil {
					t.Fatal(err)
				}
				if state != string(tokenusage.ReservationPending) {
					t.Fatalf("partial recovery committed: %s", state)
				}
			}
		})
	}
}

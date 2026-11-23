package atestore

import (
	"context"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

func TestUsageTeamRecoveryLeavesOtherRuntimeUntouched(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	owned, _ := usageTestReservation(t, store)
	foreign, _ := usageTestReservation(t, store)
	if err := store.MarkDispatched(ctx, foreign.ID); err != nil {
		t.Fatal(err)
	}
	if count, err := store.RecoverTeamReservations(ctx, owned.TeamID); err != nil || count != 1 {
		t.Fatalf("team recovery: %d %v", count, err)
	}
	if count, err := store.RecoverTeamReservations(ctx, owned.TeamID); err != nil || count != 0 {
		t.Fatalf("team recovery retry: %d %v", count, err)
	}
	if count, err := store.RecoverTeamReservations(ctx, ""); err == nil || count != 0 {
		t.Fatalf("empty team accepted: %d %v", count, err)
	}
	if err := store.authority.View(ctx, func(tx *ateauthority.Tx) error {
		own, err := getRecord[usageReservation](tx, "usage-reservation", owned.ID)
		if err != nil {
			return err
		}
		other, err := getRecord[usageReservation](tx, "usage-reservation", foreign.ID)
		if err != nil {
			return err
		}
		if own.State != tokenusage.ReservationReleased {
			t.Errorf("unsent owned reservation was not released: %+v", own)
		}
		if other.State != tokenusage.ReservationPending || !other.Dispatched {
			t.Errorf("other runtime changed: %+v", other)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	budget, err := store.Budget(ctx, foreign.TeamID)
	if err != nil || budget.ReservedTotalTokens != 40 || budget.TotalTokens != 0 {
		t.Fatalf("foreign budget changed: %+v %v", budget, err)
	}
	if count, err := store.RecoverTeamReservations(ctx, foreign.TeamID); err != nil || count != 1 {
		t.Fatalf("foreign runtime recovery: %d %v", count, err)
	}
	budget, err = store.Budget(ctx, foreign.TeamID)
	if err != nil || budget.ReservedTotalTokens != 0 || budget.TotalTokens != 40 || budget.CostMicros != 50 {
		t.Fatalf("unknown dispatch was refunded: %+v %v", budget, err)
	}
}

func TestUsageAbsentReceiptRemainsAbsentAfterNativeRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	reservation, _ := usageTestReservation(t, store)
	if err := store.authority.View(ctx, func(tx *ateauthority.Tx) error {
		stored, err := getRecord[usageReservation](tx, "usage-reservation", reservation.ID)
		if err != nil {
			return err
		}
		if len(stored.Outcome) != 0 {
			t.Errorf("absent receipt decoded as %q", stored.Outcome)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReleaseReservation(ctx, reservation.ID, "not dispatched"); err != nil {
		t.Fatal(err)
	}
}

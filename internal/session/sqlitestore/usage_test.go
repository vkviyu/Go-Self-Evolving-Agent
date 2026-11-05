package sqlitestore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

func TestUsageLedgerIsIdempotentAndEnforcesTeamBudget(t *testing.T) {
	database, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: "usage-root"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConfigureBudget(context.Background(), root.TeamID, tokenusage.BudgetLimits{MaxTotalTokens: 10}); err != nil {
		t.Fatal(err)
	}
	record := tokenusage.Record{RecordID: "usage-record-1", TeamID: root.TeamID, AgentID: root.AgentID, TurnID: "turn-1", RequestID: "turn-1:1", Usage: tokenusage.Snapshot{Input: 6, Cached: 2, Output: 4, Total: 10}}
	if err := store.RecordUsage(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordUsage(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	budget, err := store.Budget(context.Background(), root.TeamID)
	if err != nil {
		t.Fatal(err)
	}
	if budget.TotalTokens != 10 || !budget.Exhausted {
		t.Fatalf("unexpected budget snapshot: %#v", budget)
	}
	allowed, err := store.CanAdmit(context.Background(), root.TeamID)
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Fatal("exhausted team budget admitted a new run")
	}
	report, err := store.UsageReport(context.Background(), tokenusage.UsageQuery{TeamID: root.TeamID, AgentID: root.AgentID})
	if err != nil {
		t.Fatal(err)
	}
	if report.Aggregate.Records != 1 || report.Aggregate.CachedTokens != 2 || report.Aggregate.CostMicros != 0 {
		t.Fatalf("unexpected usage report: %#v", report)
	}

	priced := tokenusage.NewPriceTable(map[string]tokenusage.Price{
		"test/provider-model": {InputMicrosPerMillion: 1_000_000, CachedInputMicrosPerMillion: 100_000, OutputMicrosPerMillion: 2_000_000},
	})
	cost, ok := priced.CostMicros("test", "provider-model", tokenusage.Snapshot{Input: 6, Cached: 2, Output: 4})
	if !ok || cost != 12 {
		t.Fatalf("unexpected calculated cost: %d known=%v", cost, ok)
	}
	if _, ok := priced.CostMicros("unknown", "model", tokenusage.Snapshot{Input: 1}); ok {
		t.Fatal("unknown model unexpectedly received a price")
	}
	if err := store.RecordUsage(context.Background(), tokenusage.Record{RecordID: "usage-record-2", TeamID: root.TeamID, AgentID: root.AgentID, RequestID: "turn-2:1", RecordedAt: time.Now(), CostMicros: 12, Quality: "provider", Usage: tokenusage.Snapshot{Input: 1, Output: 1, Total: 2}}); err != nil {
		t.Fatal(err)
	}
	report, err = store.UsageReport(context.Background(), tokenusage.UsageQuery{TeamID: root.TeamID, Provider: ""})
	if err != nil || report.Aggregate.CostMicros != 12 {
		t.Fatalf("priced usage report = %#v err=%v", report, err)
	}
}

func TestUsageReservationsPreventOversubscriptionAndSettleIdempotently(t *testing.T) {
	database, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: "reservation-root"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConfigureBudget(context.Background(), root.TeamID, tokenusage.BudgetLimits{MaxTotalTokens: 10}); err != nil {
		t.Fatal(err)
	}
	reservation, err := store.Reserve(context.Background(), tokenusage.ReservationRequest{
		TeamID: root.TeamID, AgentID: root.AgentID, RequestID: "request-1", InputTokens: 4, OutputTokens: 6, TotalTokens: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if reservation.State != tokenusage.ReservationPending {
		t.Fatalf("reservation state = %s", reservation.State)
	}
	if _, err := store.Reserve(context.Background(), tokenusage.ReservationRequest{TeamID: root.TeamID, AgentID: root.AgentID, RequestID: "request-2", InputTokens: 1, OutputTokens: 1, TotalTokens: 2}); !errors.Is(err, tokenusage.ErrBudgetExceeded) {
		t.Fatalf("second reservation error = %v", err)
	}
	budget, err := store.Budget(context.Background(), root.TeamID)
	if err != nil {
		t.Fatal(err)
	}
	if budget.ReservedTotalTokens != 10 || !budget.Exhausted {
		t.Fatalf("budget with reservation = %#v", budget)
	}
	record := tokenusage.Record{TeamID: root.TeamID, AgentID: root.AgentID, RequestID: "request-1", Usage: tokenusage.Snapshot{Input: 3, Output: 4, Total: 7}}
	if err := store.SettleReservation(context.Background(), reservation.ID, record); err != nil {
		t.Fatal(err)
	}
	if err := store.SettleReservation(context.Background(), reservation.ID, record); err != nil {
		t.Fatal(err)
	}
	budget, err = store.Budget(context.Background(), root.TeamID)
	if err != nil {
		t.Fatal(err)
	}
	if budget.ReservedTotalTokens != 0 || budget.TotalTokens != 7 {
		t.Fatalf("settled budget = %#v", budget)
	}
	if _, err := store.Reserve(context.Background(), tokenusage.ReservationRequest{TeamID: root.TeamID, AgentID: root.AgentID, RequestID: "request-3", InputTokens: 1, OutputTokens: 1, TotalTokens: 2}); err != nil {
		t.Fatal(err)
	}
}

func TestUsageSettlementRejectsDifferentPayloadAndRecoversUnsentReservations(t *testing.T) {
	database, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: "reservation-integrity-root"})
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := store.Reserve(context.Background(), tokenusage.ReservationRequest{TeamID: root.TeamID, AgentID: root.AgentID, RequestID: "request-integrity", InputTokens: 2, OutputTokens: 2, TotalTokens: 4, Model: "model-a"})
	if err != nil {
		t.Fatal(err)
	}
	first := tokenusage.Record{TeamID: root.TeamID, AgentID: root.AgentID, RequestID: "request-integrity", Model: "model-a", Usage: tokenusage.Snapshot{Input: 2, Output: 1, Total: 3}}
	if err := store.SettleReservation(context.Background(), reservation.ID, first); err != nil {
		t.Fatal(err)
	}
	different := first
	different.Usage.Output = 2
	different.Usage.Total = 4
	if err := store.SettleReservation(context.Background(), reservation.ID, different); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("different settlement error = %v", err)
	}
	stale, err := store.Reserve(context.Background(), tokenusage.ReservationRequest{TeamID: root.TeamID, AgentID: root.AgentID, RequestID: "request-stale", InputTokens: 1, OutputTokens: 1, TotalTokens: 2})
	if err != nil {
		t.Fatal(err)
	}
	count, err := store.RecoverReservations(context.Background())
	if err != nil || count != 1 {
		t.Fatalf("recovered reservations = %d err=%v", count, err)
	}
	if err := store.ReleaseReservation(context.Background(), stale.ID, "repeat"); err != nil {
		t.Fatal(err)
	}
}

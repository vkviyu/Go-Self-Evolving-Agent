package atestore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

func usageTestOwner(t *testing.T, store *Store) (orchestration.TeamID, orchestration.AgentID) {
	t.Helper()
	team, err := orchestration.NewTeamID()
	if err != nil {
		t.Fatal(err)
	}
	agent, err := orchestration.NewAgentID()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.authority.Update(context.Background(), func(tx *ateauthority.Tx) error {
		if err := createRecord(tx, "team", string(team), orchestration.TeamSnapshot{ID: team, RootAgentID: agent}); err != nil {
			return err
		}
		return createRecord(tx, "agent", string(agent), orchestration.AgentSnapshot{ID: agent, TeamID: team})
	}); err != nil {
		t.Fatal(err)
	}
	return team, agent
}

func usageTestReservation(t *testing.T, store *Store) (tokenusage.Reservation, tokenusage.Record) {
	t.Helper()
	team, agent := usageTestOwner(t, store)
	reservation, err := store.Reserve(context.Background(), tokenusage.ReservationRequest{
		TeamID: team, AgentID: agent, TurnID: "turn", RequestID: "turn:7",
		ModelProfile: "primary", Provider: "test", Model: "model",
		InputTokens: 30, OutputTokens: 10, TotalTokens: 40, CostMicros: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	return reservation, tokenusage.Record{TeamID: team, AgentID: agent, TurnID: "turn", RequestID: "turn:7",
		ModelProfile: "primary", Provider: "test", Model: "model",
		Usage: tokenusage.Snapshot{Input: 5, Cached: 2, Output: 2, Reasoning: 1, Total: 7}, CostMicros: 9}
}

func TestUsageReservationBudgetAndIdempotency(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	team, agent := usageTestOwner(t, store)
	if err := store.ConfigureBudget(nil, team, tokenusage.BudgetLimits{MaxTotalTokens: 10, MaxCostMicros: 20}); err != nil {
		t.Fatal(err)
	}
	request := tokenusage.ReservationRequest{TeamID: team, AgentID: agent, RequestID: "request", InputTokens: 7, OutputTokens: 3, CostMicros: 20}
	reserved, err := store.Reserve(nil, request)
	if err != nil || reserved.TotalTokens != 10 {
		t.Fatalf("reserve: %+v %v", reserved, err)
	}
	if allowed, err := store.CanAdmit(nil, team); err != nil || allowed {
		t.Fatalf("full budget admitted: %t %v", allowed, err)
	}
	if retry, err := store.Reserve(ctx, request); err != nil || retry.ID != reserved.ID {
		t.Fatalf("full budget rejected retry: %+v %v", retry, err)
	}
	request.OutputTokens++
	if _, err := store.Reserve(ctx, request); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("reservation payload conflict: %v", err)
	}
	request.RequestID = "another"
	if _, err := store.Reserve(ctx, request); !errors.Is(err, tokenusage.ErrBudgetExceeded) {
		t.Fatalf("budget overspent: %v", err)
	}
	if err := store.ReleaseReservation(ctx, reserved.ID, "not sent"); err != nil {
		t.Fatal(err)
	}
	if err := store.ReleaseReservation(ctx, reserved.ID, "retry"); err != nil {
		t.Fatal(err)
	}
	if allowed, err := store.CanAdmit(ctx, team); err != nil || !allowed {
		t.Fatalf("released budget not returned: %t %v", allowed, err)
	}
	request.RequestID, request.OutputTokens = "request", 3
	if retry, err := store.Reserve(ctx, request); err != nil || retry.State != tokenusage.ReservationReleased {
		t.Fatalf("released identity reused: %+v %v", retry, err)
	}
	record := tokenusage.Record{TeamID: team, AgentID: agent, RequestID: request.RequestID}
	if err := store.SettleReservation(ctx, reserved.ID, record); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("released reservation settled: %v", err)
	}
}

func TestUsageReservationEnforcesExecutionBudgetAcrossRecovery(t *testing.T) {
	store := newTestStore(t)
	team, agent := usageTestOwner(t, store)
	hash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	first, err := store.Reserve(context.Background(), tokenusage.ReservationRequest{
		TeamID: team, AgentID: agent, RunID: "execution-1", TurnID: "turn-1", RequestID: "request-1",
		ContractHash: hash, ExecutionBudget: tokenusage.BudgetLimits{MaxTotalTokens: 10, MaxCostMicros: 20},
		InputTokens: 4, OutputTokens: 2, TotalTokens: 6, CostMicros: 12,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Reserve(context.Background(), tokenusage.ReservationRequest{
		TeamID: team, AgentID: agent, RunID: "execution-1", TurnID: "turn-2", RequestID: "request-2",
		ContractHash: hash, ExecutionBudget: tokenusage.BudgetLimits{MaxTotalTokens: 10, MaxCostMicros: 20},
		InputTokens: 3, OutputTokens: 2, TotalTokens: 5, CostMicros: 8,
	}); !errors.Is(err, tokenusage.ErrBudgetExceeded) {
		t.Fatalf("pending execution budget was bypassed: %v", err)
	}
	if err := store.SettleReservation(context.Background(), first.ID, tokenusage.Record{
		TeamID: team, AgentID: agent, RunID: "execution-1", TurnID: "turn-1", RequestID: "request-1",
		Usage: tokenusage.Snapshot{Input: 4, Output: 2, Total: 6}, CostMicros: 12,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Reserve(context.Background(), tokenusage.ReservationRequest{
		TeamID: team, AgentID: agent, RunID: "execution-1", TurnID: "turn-3", RequestID: "request-3",
		ContractHash: hash, ExecutionBudget: tokenusage.BudgetLimits{MaxTotalTokens: 10, MaxCostMicros: 20},
		InputTokens: 3, OutputTokens: 2, TotalTokens: 5, CostMicros: 8,
	}); !errors.Is(err, tokenusage.ErrBudgetExceeded) {
		t.Fatalf("settled execution budget was bypassed after recovery: %v", err)
	}
}

func TestUsageDispatchReceiptAndSettlementPolicies(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	reserved, record := usageTestReservation(t, store)
	if err := store.StageSettlement(ctx, reserved.ID, record); err == nil {
		t.Fatal("receipt accepted before dispatch")
	}
	if err := store.MarkDispatched(ctx, reserved.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDispatched(ctx, reserved.ID); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("dispatch repeated: %v", err)
	}
	if err := store.ReleaseReservation(ctx, reserved.ID, "send failed"); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("unknown dispatch refunded: %v", err)
	}
	wrong := record
	wrong.Model = "other"
	if err := store.StageSettlement(ctx, reserved.ID, wrong); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("model mismatch accepted: %v", err)
	}
	if err := store.StageSettlement(ctx, reserved.ID, record); err != nil {
		t.Fatal(err)
	}
	if err := store.StageSettlement(ctx, reserved.ID, record); err != nil {
		t.Fatal(err)
	}
	wrong = record
	wrong.Usage.Total++
	if err := store.StageSettlement(ctx, reserved.ID, wrong); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("receipt overwritten: %v", err)
	}
	if err := store.SettleReservation(ctx, reserved.ID, wrong); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("settlement ignored receipt: %v", err)
	}
	if err := store.SettleReservation(ctx, reserved.ID, record); err != nil {
		t.Fatal(err)
	}
	if err := store.SettleReservation(ctx, reserved.ID, record); err != nil {
		t.Fatal(err)
	}
	if err := store.StageSettlement(ctx, reserved.ID, record); err != nil {
		t.Fatal(err)
	}
	budget, err := store.Budget(ctx, reserved.TeamID)
	if err != nil || budget.TotalTokens != 7 || budget.CostMicros != 9 || budget.ReservedTotalTokens != 0 {
		t.Fatalf("settled budget: %+v %v", budget, err)
	}
	report, err := store.UsageReport(ctx, tokenusage.UsageQuery{TeamID: reserved.TeamID})
	if err != nil || report.Aggregate.Records != 1 || report.Aggregate.CachedTokens != 2 || report.Aggregate.ReasoningTokens != 1 {
		t.Fatalf("duplicate charge or lost usage: %+v %v", report, err)
	}
}

func TestUsageReportFiltersAndRecordIdentity(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	team, agent := usageTestOwner(t, store)
	now := time.Now().UTC()
	record := tokenusage.Record{RecordID: "record", TeamID: team, AgentID: agent, RequestID: "one",
		Provider: "provider", Model: "model", Usage: tokenusage.Snapshot{Input: 4, Output: 2, Total: 6}, RecordedAt: now}
	if err := store.RecordUsage(nil, record); err != nil {
		t.Fatal(err)
	}
	retry := record
	retry.RecordedAt = now.Add(time.Hour)
	if err := store.RecordUsage(ctx, retry); err != nil {
		t.Fatalf("timestamp included in request identity: %v", err)
	}
	retry.Usage.Total++
	if err := store.RecordUsage(ctx, retry); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("record overwritten: %v", err)
	}
	duplicateID := record
	duplicateID.RequestID = "two"
	if err := store.RecordUsage(ctx, duplicateID); err == nil {
		t.Fatal("record primary key reused")
	}
	for _, query := range []tokenusage.UsageQuery{
		{TeamID: team, Until: now}, {TeamID: team, Since: now.Add(time.Nanosecond)},
		{TeamID: team, Provider: "other"}, {TeamID: team, Model: "other"},
	} {
		report, err := store.UsageReport(ctx, query)
		if err != nil || report.Aggregate.Records != 0 {
			t.Fatalf("filter %+v: %+v %v", query, report, err)
		}
	}
	report, err := store.UsageReport(ctx, tokenusage.UsageQuery{TeamID: team, AgentID: agent, Provider: "provider", Model: "model", Since: now, Until: now.Add(time.Nanosecond)})
	if err != nil || report.Aggregate.Records != 1 || report.Aggregate.TotalTokens != 6 {
		t.Fatalf("inclusive/exclusive bounds: %+v %v", report, err)
	}
}

func TestUsageConcurrentReservationsCannotOverspend(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	team, agent := usageTestOwner(t, store)
	if err := store.ConfigureBudget(ctx, team, tokenusage.BudgetLimits{MaxTotalTokens: 10}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := store.Reserve(ctx, tokenusage.ReservationRequest{TeamID: team, AgentID: agent, RequestID: fmt.Sprint(i), TotalTokens: 7})
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		} else if !errors.Is(err, tokenusage.ErrBudgetExceeded) {
			t.Fatalf("concurrent reservation error: %v", err)
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted %d requests", accepted)
	}
	budget, err := store.Budget(ctx, team)
	if err != nil || budget.ReservedTotalTokens != 7 {
		t.Fatalf("concurrent budget: %+v %v", budget, err)
	}
}

func TestUsageForeignKeysAndValidation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	missingTeam, _ := orchestration.NewTeamID()
	missingAgent, _ := orchestration.NewAgentID()
	if err := store.ConfigureBudget(ctx, missingTeam, tokenusage.BudgetLimits{}); !errors.Is(err, orchestration.ErrNotFound) {
		t.Fatalf("missing budget team: %v", err)
	}
	if _, err := store.Reserve(ctx, tokenusage.ReservationRequest{TeamID: missingTeam, AgentID: missingAgent, RequestID: "missing"}); !errors.Is(err, orchestration.ErrNotFound) {
		t.Fatalf("missing reservation references: %v", err)
	}
	if err := store.RecordUsage(ctx, tokenusage.Record{TeamID: missingTeam, AgentID: missingAgent, RequestID: "missing"}); !errors.Is(err, orchestration.ErrNotFound) {
		t.Fatalf("missing ledger references: %v", err)
	}
	team, agent := usageTestOwner(t, store)
	if err := store.ConfigureBudget(ctx, team, tokenusage.BudgetLimits{MaxTotalTokens: -1}); err == nil {
		t.Fatal("negative budget accepted")
	}
	if _, err := store.Reserve(ctx, tokenusage.ReservationRequest{TeamID: team, AgentID: agent, RequestID: "negative", CostMicros: -1}); err == nil {
		t.Fatal("negative reservation accepted")
	}
	if err := store.RecordUsage(ctx, tokenusage.Record{TeamID: team, AgentID: agent, RequestID: "negative", Usage: tokenusage.Snapshot{Cached: -1}}); err == nil {
		t.Fatal("negative receipt accepted")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.Reserve(canceled, tokenusage.ReservationRequest{TeamID: team, AgentID: agent, RequestID: "canceled"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled operation: %v", err)
	}
}

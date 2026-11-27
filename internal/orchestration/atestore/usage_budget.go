package atestore

import (
	"context"
	"errors"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

// SQL SUM rejects overflowing totals. Do not let Go wrap a budget or report.
func usageAdd[T ~int | ~int64](a, b T) (T, error) {
	sum := a + b
	if a < 0 || b < 0 || sum < a {
		return 0, errors.New("usage aggregate overflow or negative amount")
	}
	return sum, nil
}

// usageWouldExceed checks new usage against the limit after accounting for reservations,
// without summing potentially overflowing counts. A nonpositive limit is uncapped.
func usageWouldExceed(limit, used, reserved, requested int64) bool {
	if limit <= 0 {
		return false
	}
	return used > limit || reserved > limit-used || requested > limit-used-reserved
}

func usageExhausted(snapshot tokenusage.BudgetSnapshot) bool {
	return usageWouldExceed(int64(snapshot.Limits.MaxInputTokens), int64(snapshot.InputTokens), int64(snapshot.ReservedInputTokens), 1) ||
		usageWouldExceed(int64(snapshot.Limits.MaxOutputTokens), int64(snapshot.OutputTokens), int64(snapshot.ReservedOutputTokens), 1) ||
		usageWouldExceed(int64(snapshot.Limits.MaxTotalTokens), int64(snapshot.TotalTokens), int64(snapshot.ReservedTotalTokens), 1) ||
		usageWouldExceed(snapshot.Limits.MaxCostMicros, snapshot.CostMicros, snapshot.ReservedCostMicros, 1)
}

func (store *Store) ConfigureBudget(ctx context.Context, id orchestration.TeamID, limits tokenusage.BudgetLimits) error {
	if err := id.Validate(); err != nil {
		return err
	}
	if limits.MaxInputTokens < 0 || limits.MaxOutputTokens < 0 || limits.MaxTotalTokens < 0 || limits.MaxCostMicros < 0 {
		return errors.New("budget limits must be non-negative")
	}
	return store.authority.Update(usageContext(ctx), func(tx *ateauthority.Tx) error {
		if _, err := getRecord[orchestration.TeamSnapshot](tx, "team", string(id)); err != nil {
			return err
		}
		return putRecord(tx, "usage-budget", string(id), usageBudget{Limits: limits, UpdatedAt: time.Now().UTC()})
	})
}

func usageBudgetTx(tx *ateauthority.Tx, id orchestration.TeamID) (tokenusage.BudgetSnapshot, error) {
	snapshot := tokenusage.BudgetSnapshot{TeamID: id, UpdatedAt: time.Now().UTC()}
	budget, err := getRecord[usageBudget](tx, "usage-budget", string(id))
	if err == nil {
		snapshot.Limits, snapshot.UpdatedAt = budget.Limits, budget.UpdatedAt
	} else if !errors.Is(err, orchestration.ErrNotFound) {
		return snapshot, err
	}
	records, err := listRecords[tokenusage.Record](tx, "usage-record")
	if err != nil {
		return snapshot, err
	}
	for _, record := range records {
		if record.TeamID != id {
			continue
		}
		if snapshot.InputTokens, err = usageAdd(snapshot.InputTokens, record.Usage.Input); err != nil {
			return snapshot, err
		}
		if snapshot.OutputTokens, err = usageAdd(snapshot.OutputTokens, record.Usage.Output); err != nil {
			return snapshot, err
		}
		if snapshot.TotalTokens, err = usageAdd(snapshot.TotalTokens, record.Usage.Total); err != nil {
			return snapshot, err
		}
		if snapshot.CostMicros, err = usageAdd(snapshot.CostMicros, record.CostMicros); err != nil {
			return snapshot, err
		}
	}
	reservations, err := listRecords[usageReservation](tx, "usage-reservation")
	if err != nil {
		return snapshot, err
	}
	for _, reservation := range reservations {
		if reservation.TeamID != id || reservation.State != tokenusage.ReservationPending {
			continue
		}
		if snapshot.ReservedInputTokens, err = usageAdd(snapshot.ReservedInputTokens, reservation.InputTokens); err != nil {
			return snapshot, err
		}
		if snapshot.ReservedOutputTokens, err = usageAdd(snapshot.ReservedOutputTokens, reservation.OutputTokens); err != nil {
			return snapshot, err
		}
		if snapshot.ReservedTotalTokens, err = usageAdd(snapshot.ReservedTotalTokens, reservation.TotalTokens); err != nil {
			return snapshot, err
		}
		if snapshot.ReservedCostMicros, err = usageAdd(snapshot.ReservedCostMicros, reservation.CostMicros); err != nil {
			return snapshot, err
		}
	}
	snapshot.Exhausted = usageExhausted(snapshot)
	return snapshot, nil
}

func (store *Store) Budget(ctx context.Context, id orchestration.TeamID) (snapshot tokenusage.BudgetSnapshot, err error) {
	if err = id.Validate(); err != nil {
		return
	}
	err = store.authority.View(usageContext(ctx), func(tx *ateauthority.Tx) error {
		snapshot, err = usageBudgetTx(tx, id)
		return err
	})
	return
}

func (store *Store) CanAdmit(ctx context.Context, id orchestration.TeamID) (bool, error) {
	snapshot, err := store.Budget(ctx, id)
	if err != nil {
		return false, err
	}
	return !snapshot.Exhausted, nil
}

// UsageReport aggregates the persisted usage records matching the query.
func (store *Store) UsageReport(ctx context.Context, query tokenusage.UsageQuery) (report tokenusage.UsageReport, err error) {
	if err = query.TeamID.Validate(); err != nil {
		return
	}
	if query.AgentID != "" {
		if err = query.AgentID.Validate(); err != nil {
			return
		}
	}
	report.Query = query
	err = store.authority.View(usageContext(ctx), func(tx *ateauthority.Tx) error {
		records, err := listRecords[tokenusage.Record](tx, "usage-record")
		if err != nil {
			return err
		}
		for _, record := range records {
			if record.TeamID != query.TeamID || (query.AgentID != "" && record.AgentID != query.AgentID) ||
				(query.Provider != "" && record.Provider != query.Provider) || (query.Model != "" && record.Model != query.Model) ||
				(!query.Since.IsZero() && record.RecordedAt.Before(query.Since)) ||
				(!query.Until.IsZero() && !record.RecordedAt.Before(query.Until)) {
				continue
			}
			aggregate := &report.Aggregate
			aggregate.Records++
			if aggregate.InputTokens, err = usageAdd(aggregate.InputTokens, record.Usage.Input); err != nil {
				return err
			}
			if aggregate.CachedTokens, err = usageAdd(aggregate.CachedTokens, record.Usage.Cached); err != nil {
				return err
			}
			if aggregate.OutputTokens, err = usageAdd(aggregate.OutputTokens, record.Usage.Output); err != nil {
				return err
			}
			if aggregate.ReasoningTokens, err = usageAdd(aggregate.ReasoningTokens, record.Usage.Reasoning); err != nil {
				return err
			}
			if aggregate.TotalTokens, err = usageAdd(aggregate.TotalTokens, record.Usage.Total); err != nil {
				return err
			}
			if aggregate.CostMicros, err = usageAdd(aggregate.CostMicros, record.CostMicros); err != nil {
				return err
			}
		}
		return nil
	})
	return
}

package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

func hashReservationRequest(request tokenusage.ReservationRequest) string {
	canonical := struct {
		TeamID       orchestration.TeamID  `json:"team_id"`
		AgentID      orchestration.AgentID `json:"agent_id"`
		RunID        orchestration.RunID   `json:"run_id"`
		TurnID       string                `json:"turn_id"`
		RequestID    string                `json:"request_id"`
		InputTokens  int                   `json:"input_tokens"`
		OutputTokens int                   `json:"output_tokens"`
		TotalTokens  int                   `json:"total_tokens"`
		CostMicros   int64                 `json:"cost_micros"`
		ModelProfile string                `json:"model_profile"`
		Provider     string                `json:"provider"`
		Model        string                `json:"model"`
	}{request.TeamID, request.AgentID, request.RunID, request.TurnID, request.RequestID, request.InputTokens, request.OutputTokens, request.TotalTokens, request.CostMicros, request.ModelProfile, request.Provider, request.Model}
	encoded, _ := json.Marshal(canonical)
	return sha256Hex(encoded)
}

func usageRecordMatches(existing tokenusage.Record, expected tokenusage.Record) bool {
	return existing.RecordID == expected.RecordID && existing.TeamID == expected.TeamID && existing.AgentID == expected.AgentID && existing.RunID == expected.RunID && existing.TurnID == expected.TurnID && existing.RequestID == expected.RequestID && existing.ModelProfile == expected.ModelProfile && existing.Provider == expected.Provider && existing.Model == expected.Model && existing.Usage == expected.Usage && existing.CostMicros == expected.CostMicros && existing.Quality == expected.Quality
}

func existingUsageRecord(ctx context.Context, query rowQuery, teamID orchestration.TeamID, requestID string) (tokenusage.Record, error) {
	var record tokenusage.Record
	var storedTeam, storedAgent, runID, turnID, modelProfile, provider, model, quality, recordedAt string
	var usage tokenusage.Snapshot
	if err := query.QueryRowContext(ctx, `SELECT record_id, team_id, agent_id, run_id, turn_id, request_id, model_profile, provider, model, input_tokens, cached_tokens, output_tokens, reasoning_tokens, total_tokens, cost_micros, quality, recorded_at FROM usage_records WHERE team_id = ? AND request_id = ?`, string(teamID), requestID).Scan(
		&record.RecordID, &storedTeam, &storedAgent, &runID, &turnID, &record.RequestID, &modelProfile, &provider, &model, &usage.Input, &usage.Cached, &usage.Output, &usage.Reasoning, &usage.Total, &record.CostMicros, &quality, &recordedAt); err != nil {
		return tokenusage.Record{}, err
	}
	record.TeamID = orchestration.TeamID(storedTeam)
	record.AgentID = orchestration.AgentID(storedAgent)
	record.RunID = orchestration.RunID(runID)
	record.TurnID, record.ModelProfile, record.Provider, record.Model, record.Usage, record.Quality = turnID, modelProfile, provider, model, usage, quality
	var err error
	record.RecordedAt, err = time.Parse(time.RFC3339Nano, recordedAt)
	if err != nil {
		return tokenusage.Record{}, err
	}
	return record, nil
}

func (store *OrchestrationStore) RecordUsage(ctx context.Context, record tokenusage.Record) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := record.TeamID.Validate(); err != nil {
		return fmt.Errorf("usage team: %w", err)
	}
	if err := record.AgentID.Validate(); err != nil {
		return fmt.Errorf("usage Agent: %w", err)
	}
	if record.RequestID == "" {
		return errors.New("usage request ID is required")
	}
	if record.RecordID == "" {
		record.RecordID = string(record.TeamID) + "_" + record.RequestID
	}
	if record.RecordedAt.IsZero() {
		record.RecordedAt = time.Now().UTC()
	}
	usage := record.Usage
	if usage.Input < 0 || usage.Cached < 0 || usage.Output < 0 || usage.Reasoning < 0 || usage.Total < 0 {
		return errors.New("usage token counts must be non-negative")
	}
	if record.CostMicros < 0 {
		return errors.New("usage cost must be non-negative")
	}
	if record.Quality == "" {
		record.Quality = "provider"
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin usage record: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := recordUsageTx(ctx, tx, record); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit usage record: %w", err)
	}
	return nil
}

func recordUsageTx(ctx context.Context, tx *sql.Tx, record tokenusage.Record) error {
	usage := record.Usage
	if existing, err := existingUsageRecord(ctx, tx, record.TeamID, record.RequestID); err == nil {
		if !usageRecordMatches(existing, record) {
			return fmt.Errorf("%w: usage request %q", orchestration.ErrIdempotencyConflict, record.RequestID)
		}
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("inspect existing usage record: %w", err)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO usage_records(record_id, team_id, agent_id, run_id, turn_id, request_id, model_profile, provider, model, input_tokens, cached_tokens, output_tokens, reasoning_tokens, total_tokens, cost_micros, quality, recorded_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(team_id, request_id) DO NOTHING`,
		record.RecordID, string(record.TeamID), string(record.AgentID), string(record.RunID), record.TurnID, record.RequestID,
		record.ModelProfile, record.Provider, record.Model, usage.Input, usage.Cached, usage.Output, usage.Reasoning, usage.Total, record.CostMicros, record.Quality, record.RecordedAt.Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("insert usage record: %w", err)
	}
	return nil
}

func (store *OrchestrationStore) Reserve(ctx context.Context, request tokenusage.ReservationRequest) (tokenusage.Reservation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := request.TeamID.Validate(); err != nil {
		return tokenusage.Reservation{}, fmt.Errorf("reservation team: %w", err)
	}
	if err := request.AgentID.Validate(); err != nil {
		return tokenusage.Reservation{}, fmt.Errorf("reservation Agent: %w", err)
	}
	if request.RequestID == "" {
		return tokenusage.Reservation{}, errors.New("reservation request ID is required")
	}
	if request.InputTokens < 0 || request.OutputTokens < 0 || request.TotalTokens < 0 || request.CostMicros < 0 {
		return tokenusage.Reservation{}, errors.New("reservation amounts must be non-negative")
	}
	if request.TotalTokens == 0 {
		request.TotalTokens = request.InputTokens + request.OutputTokens
	}
	requestHash := hashReservationRequest(request)
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return tokenusage.Reservation{}, fmt.Errorf("begin usage reservation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var existing tokenusage.Reservation
	var state, createdAt, updatedAt string
	var storedHash string
	var modelProfile, provider, model string
	var dispatched sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT reservation_id, team_id, agent_id, run_id, turn_id, request_id, input_tokens, output_tokens, total_tokens, cost_micros, request_hash, state, created_at, updated_at, dispatched_at, model_profile, provider, model FROM usage_reservations WHERE team_id = ? AND request_id = ?`, string(request.TeamID), request.RequestID).Scan(
		&existing.ID, &existing.TeamID, &existing.AgentID, &existing.RunID, &existing.TurnID, &existing.RequestID, &existing.InputTokens, &existing.OutputTokens, &existing.TotalTokens, &existing.CostMicros, &storedHash, &state, &createdAt, &updatedAt, &dispatched, &modelProfile, &provider, &model)
	if err == nil {
		legacyMatches := existing.AgentID == request.AgentID && existing.RunID == request.RunID && existing.TurnID == request.TurnID &&
			existing.InputTokens == request.InputTokens && existing.OutputTokens == request.OutputTokens && existing.TotalTokens == request.TotalTokens && existing.CostMicros == request.CostMicros &&
			(modelProfile == "" || modelProfile == request.ModelProfile) && (provider == "" || provider == request.Provider) && (model == "" || model == request.Model)
		if (storedHash != "" && storedHash != requestHash) || (storedHash == "" && !legacyMatches) {
			return tokenusage.Reservation{}, fmt.Errorf("%w: reservation request %q", orchestration.ErrIdempotencyConflict, request.RequestID)
		}
		existing.RequestHash = storedHash
		existing.State = tokenusage.ReservationState(state)
		existing.Dispatched = dispatched.Valid
		existing.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return tokenusage.Reservation{}, err
		}
		existing.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
		if err != nil {
			return tokenusage.Reservation{}, err
		}
		if err := tx.Commit(); err != nil {
			return tokenusage.Reservation{}, err
		}
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return tokenusage.Reservation{}, fmt.Errorf("inspect usage reservation: %w", err)
	}
	var limits tokenusage.BudgetLimits
	var ignoredUpdatedAt string
	budgetErr := tx.QueryRowContext(ctx, `SELECT max_input_tokens, max_output_tokens, max_total_tokens, max_cost_micros, updated_at FROM team_budgets WHERE team_id = ?`, string(request.TeamID)).Scan(&limits.MaxInputTokens, &limits.MaxOutputTokens, &limits.MaxTotalTokens, &limits.MaxCostMicros, &ignoredUpdatedAt)
	if budgetErr != nil && !errors.Is(budgetErr, sql.ErrNoRows) {
		return tokenusage.Reservation{}, fmt.Errorf("load reservation budget: %w", budgetErr)
	}
	var usedInput, usedOutput, usedTotal int
	var usedCost int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(total_tokens), 0), COALESCE(SUM(cost_micros), 0) FROM usage_records WHERE team_id = ?`, string(request.TeamID)).Scan(&usedInput, &usedOutput, &usedTotal, &usedCost); err != nil {
		return tokenusage.Reservation{}, fmt.Errorf("sum reservation usage: %w", err)
	}
	var reservedInput, reservedOutput, reservedTotal int
	var reservedCost int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(total_tokens), 0), COALESCE(SUM(cost_micros), 0) FROM usage_reservations WHERE team_id = ? AND state = ?`, string(request.TeamID), string(tokenusage.ReservationPending)).Scan(&reservedInput, &reservedOutput, &reservedTotal, &reservedCost); err != nil {
		return tokenusage.Reservation{}, fmt.Errorf("sum pending reservations: %w", err)
	}
	if limits.MaxInputTokens > 0 && usedInput+reservedInput+request.InputTokens > limits.MaxInputTokens ||
		limits.MaxOutputTokens > 0 && usedOutput+reservedOutput+request.OutputTokens > limits.MaxOutputTokens ||
		limits.MaxTotalTokens > 0 && usedTotal+reservedTotal+request.TotalTokens > limits.MaxTotalTokens ||
		limits.MaxCostMicros > 0 && usedCost+reservedCost+request.CostMicros > limits.MaxCostMicros {
		return tokenusage.Reservation{}, tokenusage.ErrBudgetExceeded
	}
	reservationID, err := randomID()
	if err != nil {
		return tokenusage.Reservation{}, err
	}
	reservationID = "reservation-" + reservationID
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO usage_reservations(reservation_id, team_id, agent_id, run_id, turn_id, request_id, model_profile, provider, model, input_tokens, output_tokens, total_tokens, cost_micros, request_hash, state, lifecycle_version, created_at, updated_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`, reservationID, string(request.TeamID), string(request.AgentID), string(request.RunID), request.TurnID, request.RequestID, request.ModelProfile, request.Provider, request.Model, request.InputTokens, request.OutputTokens, request.TotalTokens, request.CostMicros, requestHash, string(tokenusage.ReservationPending), now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		return tokenusage.Reservation{}, fmt.Errorf("insert usage reservation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return tokenusage.Reservation{}, fmt.Errorf("commit usage reservation: %w", err)
	}
	return tokenusage.Reservation{ID: reservationID, TeamID: request.TeamID, AgentID: request.AgentID, RunID: request.RunID, TurnID: request.TurnID, RequestID: request.RequestID, InputTokens: request.InputTokens, OutputTokens: request.OutputTokens, TotalTokens: request.TotalTokens, CostMicros: request.CostMicros, RequestHash: requestHash, State: tokenusage.ReservationPending, CreatedAt: now, UpdatedAt: now}, nil
}

func (store *OrchestrationStore) SettleReservation(ctx context.Context, reservationID string, record tokenusage.Record) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(reservationID) == "" {
		return errors.New("reservation ID is required")
	}
	if err := record.TeamID.Validate(); err != nil {
		return fmt.Errorf("settlement team: %w", err)
	}
	if err := record.AgentID.Validate(); err != nil {
		return fmt.Errorf("settlement Agent: %w", err)
	}
	if record.RequestID == "" {
		return errors.New("settlement request ID is required")
	}
	if record.Usage.Input < 0 || record.Usage.Cached < 0 || record.Usage.Output < 0 || record.Usage.Reasoning < 0 || record.Usage.Total < 0 || record.CostMicros < 0 {
		return errors.New("settlement usage amounts must be non-negative")
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
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var teamID, agentID, state, requestID string
	var receipt []byte
	if err := tx.QueryRowContext(ctx, `SELECT team_id, agent_id, state, request_id, outcome FROM usage_reservations WHERE reservation_id = ?`, reservationID).Scan(&teamID, &agentID, &state, &requestID, &receipt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: reservation %s", orchestration.ErrNotFound, reservationID)
		}
		return err
	}
	if teamID != string(record.TeamID) || agentID != string(record.AgentID) || requestID != record.RequestID {
		return fmt.Errorf("%w: reservation owner or request mismatch", orchestration.ErrInvalidEvent)
	}
	if len(receipt) > 0 {
		var staged tokenusage.Record
		if err := json.Unmarshal(receipt, &staged); err != nil {
			return err
		}
		if !usageRecordMatches(staged, record) {
			return fmt.Errorf("%w: staged settlement differs", orchestration.ErrIdempotencyConflict)
		}
	}
	if existing, lookupErr := existingUsageRecord(ctx, tx, record.TeamID, record.RequestID); lookupErr == nil {
		if !usageRecordMatches(existing, record) {
			return fmt.Errorf("%w: settlement request %q", orchestration.ErrIdempotencyConflict, record.RequestID)
		}
	} else if !errors.Is(lookupErr, sql.ErrNoRows) {
		return fmt.Errorf("inspect settlement record: %w", lookupErr)
	}
	if tokenusage.ReservationState(state) == tokenusage.ReservationSettled {
		return tx.Commit()
	}
	if tokenusage.ReservationState(state) == tokenusage.ReservationReleased {
		return fmt.Errorf("%w: reservation was released", orchestration.ErrInvalidEvent)
	}
	if err := recordUsageTx(ctx, tx, record); err != nil {
		return err
	}
	if err := execCAS(ctx, tx, orchestration.ErrSequenceConflict, `UPDATE usage_reservations SET state = ?, updated_at = ? WHERE reservation_id = ? AND state = ?`, string(tokenusage.ReservationSettled), time.Now().UTC().Format(time.RFC3339Nano), reservationID, string(tokenusage.ReservationPending)); err != nil {
		return fmt.Errorf("settle usage reservation: %w", err)
	}
	return tx.Commit()
}

func (store *OrchestrationStore) ReleaseReservation(ctx context.Context, reservationID string, reason string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(reservationID) == "" {
		return errors.New("reservation ID is required")
	}
	// A failed/canceled dispatch may have reached the provider. Only a durable
	// pre-dispatch state proves that returning its allowance is safe.
	result, err := store.db.ExecContext(ctx, `UPDATE usage_reservations SET state = ?, updated_at = ? WHERE reservation_id = ? AND state = ? AND lifecycle_version = 1 AND dispatched_at IS NULL AND outcome IS NULL`, string(tokenusage.ReservationReleased), time.Now().UTC().Format(time.RFC3339Nano), reservationID, string(tokenusage.ReservationPending))
	if err != nil {
		return fmt.Errorf("release usage reservation: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect released reservation: %w", err)
	}
	if affected == 0 {
		var state string
		if err := store.db.QueryRowContext(ctx, `SELECT state FROM usage_reservations WHERE reservation_id = ?`, reservationID).Scan(&state); errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: reservation %s", orchestration.ErrNotFound, reservationID)
		} else if err != nil {
			return err
		} else if tokenusage.ReservationState(state) == tokenusage.ReservationReleased || tokenusage.ReservationState(state) == tokenusage.ReservationSettled {
			return nil
		}
		return fmt.Errorf("%w: cannot release reservation (%s)", orchestration.ErrInvalidEvent, reason)
	}
	return nil
}

func (store *OrchestrationStore) UsageReport(ctx context.Context, query tokenusage.UsageQuery) (tokenusage.UsageReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := query.TeamID.Validate(); err != nil {
		return tokenusage.UsageReport{}, fmt.Errorf("usage query team: %w", err)
	}
	conditions := []string{"team_id = ?"}
	args := []any{string(query.TeamID)}
	if query.AgentID != "" {
		if err := query.AgentID.Validate(); err != nil {
			return tokenusage.UsageReport{}, fmt.Errorf("usage query Agent: %w", err)
		}
		conditions = append(conditions, "agent_id = ?")
		args = append(args, string(query.AgentID))
	}
	if query.Provider != "" {
		conditions = append(conditions, "provider = ?")
		args = append(args, query.Provider)
	}
	if query.Model != "" {
		conditions = append(conditions, "model = ?")
		args = append(args, query.Model)
	}
	if !query.Since.IsZero() {
		conditions = append(conditions, "recorded_at >= ?")
		args = append(args, query.Since.UTC().Format(time.RFC3339Nano))
	}
	if !query.Until.IsZero() {
		conditions = append(conditions, "recorded_at < ?")
		args = append(args, query.Until.UTC().Format(time.RFC3339Nano))
	}
	var aggregate tokenusage.UsageAggregate
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(input_tokens), 0), COALESCE(SUM(cached_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(reasoning_tokens), 0), COALESCE(SUM(total_tokens), 0), COALESCE(SUM(cost_micros), 0) FROM usage_records WHERE `+strings.Join(conditions, " AND "), args...).Scan(
		&aggregate.Records, &aggregate.InputTokens, &aggregate.CachedTokens, &aggregate.OutputTokens, &aggregate.ReasoningTokens, &aggregate.TotalTokens, &aggregate.CostMicros); err != nil {
		return tokenusage.UsageReport{}, fmt.Errorf("aggregate usage: %w", err)
	}
	return tokenusage.UsageReport{Query: query, Aggregate: aggregate}, nil
}

func (store *OrchestrationStore) ConfigureBudget(ctx context.Context, teamID orchestration.TeamID, limits tokenusage.BudgetLimits) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := teamID.Validate(); err != nil {
		return fmt.Errorf("budget team: %w", err)
	}
	if limits.MaxInputTokens < 0 || limits.MaxOutputTokens < 0 || limits.MaxTotalTokens < 0 || limits.MaxCostMicros < 0 {
		return errors.New("budget limits must be non-negative")
	}
	_, err := store.db.ExecContext(ctx, `INSERT INTO team_budgets(team_id, max_input_tokens, max_output_tokens, max_total_tokens, max_cost_micros, updated_at) VALUES(?, ?, ?, ?, ?, ?)
ON CONFLICT(team_id) DO UPDATE SET max_input_tokens=excluded.max_input_tokens, max_output_tokens=excluded.max_output_tokens, max_total_tokens=excluded.max_total_tokens, max_cost_micros=excluded.max_cost_micros, updated_at=excluded.updated_at`,
		string(teamID), limits.MaxInputTokens, limits.MaxOutputTokens, limits.MaxTotalTokens, limits.MaxCostMicros, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("configure team budget: %w", err)
	}
	return nil
}

func (store *OrchestrationStore) Budget(ctx context.Context, teamID orchestration.TeamID) (tokenusage.BudgetSnapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := teamID.Validate(); err != nil {
		return tokenusage.BudgetSnapshot{}, fmt.Errorf("budget team: %w", err)
	}
	var snapshot tokenusage.BudgetSnapshot
	var updatedAt string
	var limits tokenusage.BudgetLimits
	err := store.db.QueryRowContext(ctx, `SELECT max_input_tokens, max_output_tokens, max_total_tokens, max_cost_micros, updated_at FROM team_budgets WHERE team_id = ?`, string(teamID)).Scan(&limits.MaxInputTokens, &limits.MaxOutputTokens, &limits.MaxTotalTokens, &limits.MaxCostMicros, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		limits = tokenusage.BudgetLimits{}
		updatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	} else if err != nil {
		return tokenusage.BudgetSnapshot{}, fmt.Errorf("load team budget: %w", err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(total_tokens), 0), COALESCE(SUM(cost_micros), 0) FROM usage_records WHERE team_id = ?`, string(teamID)).Scan(&snapshot.InputTokens, &snapshot.OutputTokens, &snapshot.TotalTokens, &snapshot.CostMicros); err != nil {
		return tokenusage.BudgetSnapshot{}, fmt.Errorf("sum team usage: %w", err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(total_tokens), 0), COALESCE(SUM(cost_micros), 0) FROM usage_reservations WHERE team_id = ? AND state = ?`, string(teamID), string(tokenusage.ReservationPending)).Scan(&snapshot.ReservedInputTokens, &snapshot.ReservedOutputTokens, &snapshot.ReservedTotalTokens, &snapshot.ReservedCostMicros); err != nil {
		return tokenusage.BudgetSnapshot{}, fmt.Errorf("sum pending team reservations: %w", err)
	}
	snapshot.TeamID = teamID
	snapshot.Limits = limits
	snapshot.Exhausted = budgetExceeded(snapshot)
	snapshot.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return tokenusage.BudgetSnapshot{}, fmt.Errorf("parse budget update time: %w", err)
	}
	return snapshot, nil
}

func (store *OrchestrationStore) CanAdmit(ctx context.Context, teamID orchestration.TeamID) (bool, error) {
	snapshot, err := store.Budget(ctx, teamID)
	if err != nil {
		return false, err
	}
	return !snapshot.Exhausted, nil
}

func budgetExceeded(snapshot tokenusage.BudgetSnapshot) bool {
	if snapshot.Limits.MaxInputTokens > 0 && snapshot.InputTokens+snapshot.ReservedInputTokens >= snapshot.Limits.MaxInputTokens {
		return true
	}
	if snapshot.Limits.MaxOutputTokens > 0 && snapshot.OutputTokens+snapshot.ReservedOutputTokens >= snapshot.Limits.MaxOutputTokens {
		return true
	}
	if snapshot.Limits.MaxTotalTokens > 0 && snapshot.TotalTokens+snapshot.ReservedTotalTokens >= snapshot.Limits.MaxTotalTokens {
		return true
	}
	return snapshot.Limits.MaxCostMicros > 0 && snapshot.CostMicros+snapshot.ReservedCostMicros >= snapshot.Limits.MaxCostMicros
}

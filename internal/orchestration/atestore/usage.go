package atestore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

var _ tokenusage.DurableReservationLedger = (*Store)(nil)
var _ tokenusage.Reader = (*Store)(nil)

type usageReservation struct {
	tokenusage.Reservation
	ModelProfile     string
	Provider         string
	Model            string
	LifecycleVersion int
	// Omit absence: decoding JSON null into RawMessage produces nonempty
	// bytes, which would otherwise look like a staged provider receipt.
	Outcome json.RawMessage `json:",omitempty"`
}

type usageBudget struct {
	Limits    tokenusage.BudgetLimits
	UpdatedAt time.Time
}

// usageContext supplies a background context for usage repository calls whose caller
// omitted one.
func usageContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func usageRequestKey(teamID orchestration.TeamID, requestID string) string {
	raw, _ := json.Marshal([]string{string(teamID), requestID})
	return string(raw)
}

func usageHashRequest(request tokenusage.ReservationRequest) string {
	canonical := struct {
		TeamID          orchestration.TeamID    `json:"team_id"`
		AgentID         orchestration.AgentID   `json:"agent_id"`
		RunID           orchestration.RunID     `json:"run_id"`
		TurnID          string                  `json:"turn_id"`
		RequestID       string                  `json:"request_id"`
		ContractHash    string                  `json:"contract_hash,omitempty"`
		ExecutionBudget tokenusage.BudgetLimits `json:"execution_budget,omitempty"`
		InputTokens     int                     `json:"input_tokens"`
		OutputTokens    int                     `json:"output_tokens"`
		TotalTokens     int                     `json:"total_tokens"`
		CostMicros      int64                   `json:"cost_micros"`
		ModelProfile    string                  `json:"model_profile"`
		Provider        string                  `json:"provider"`
		Model           string                  `json:"model"`
	}{request.TeamID, request.AgentID, request.RunID, request.TurnID, request.RequestID, request.ContractHash, request.ExecutionBudget, request.InputTokens, request.OutputTokens, request.TotalTokens, request.CostMicros, request.ModelProfile, request.Provider, request.Model}
	raw, _ := json.Marshal(canonical)
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

// usageRecordMatches compares accounting identity, scope, usage and price to distinguish
// duplicate reports from conflicting ones.
func usageRecordMatches(a, b tokenusage.Record) bool {
	return a.RecordID == b.RecordID && a.TeamID == b.TeamID && a.AgentID == b.AgentID &&
		a.RunID == b.RunID && a.TurnID == b.TurnID && a.RequestID == b.RequestID &&
		a.ModelProfile == b.ModelProfile && a.Provider == b.Provider && a.Model == b.Model &&
		a.Usage == b.Usage && a.CostMicros == b.CostMicros && a.Quality == b.Quality
}

func usageNormalizeReceipt(record tokenusage.Record) (tokenusage.Record, error) {
	if err := record.TeamID.Validate(); err != nil {
		return record, err
	}
	if err := record.AgentID.Validate(); err != nil {
		return record, err
	}
	u := record.Usage
	if record.RequestID == "" || u.Input < 0 || u.Cached < 0 || u.Output < 0 || u.Reasoning < 0 || u.Total < 0 || record.CostMicros < 0 {
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

func usageExistingRecord(tx *ateauthority.Tx, teamID orchestration.TeamID, requestID string) (tokenusage.Record, error) {
	id, err := getRecord[string](tx, "usage-record-request", usageRequestKey(teamID, requestID))
	if err != nil {
		return tokenusage.Record{}, err
	}
	record, err := getRecord[tokenusage.Record](tx, "usage-record", id)
	if errors.Is(err, orchestration.ErrNotFound) {
		return record, fmt.Errorf("usage record index refers to missing record %s", id)
	}
	return record, err
}

func usageCheckReferences(tx *ateauthority.Tx, teamID orchestration.TeamID, agentID orchestration.AgentID) error {
	if _, err := getRecord[orchestration.TeamSnapshot](tx, "team", string(teamID)); err != nil {
		return err
	}
	_, err := getRecord[orchestration.AgentSnapshot](tx, "agent", string(agentID))
	return err
}

func usageRecordTx(tx *ateauthority.Tx, record tokenusage.Record) error {
	existing, err := usageExistingRecord(tx, record.TeamID, record.RequestID)
	if err == nil {
		if !usageRecordMatches(existing, record) {
			return fmt.Errorf("%w: usage request %q", orchestration.ErrIdempotencyConflict, record.RequestID)
		}
		return nil
	}
	if !errors.Is(err, orchestration.ErrNotFound) {
		return err
	}
	if err := usageCheckReferences(tx, record.TeamID, record.AgentID); err != nil {
		return err
	}
	if err := createRecord(tx, "usage-record", record.RecordID, record); err != nil {
		return err
	}
	return createRecord(tx, "usage-record-request", usageRequestKey(record.TeamID, record.RequestID), record.RecordID)
}

// RecordUsage distinguishes duplicate accounting records from conflicting records with the
// same identity.
func (store *Store) RecordUsage(ctx context.Context, record tokenusage.Record) error {
	record, err := usageNormalizeReceipt(record)
	if err != nil {
		return err
	}
	return store.authority.Update(usageContext(ctx), func(tx *ateauthority.Tx) error {
		return usageRecordTx(tx, record)
	})
}

func (store *Store) Reserve(ctx context.Context, request tokenusage.ReservationRequest) (result tokenusage.Reservation, err error) {
	if err = request.TeamID.Validate(); err != nil {
		return
	}
	if err = request.AgentID.Validate(); err != nil {
		return
	}
	if request.RequestID == "" {
		return result, errors.New("reservation request ID is required")
	}
	if request.InputTokens < 0 || request.OutputTokens < 0 || request.TotalTokens < 0 || request.CostMicros < 0 {
		return result, errors.New("reservation amounts must be non-negative")
	}
	if !executionBudgetZero(request.ExecutionBudget) {
		if request.RunID == "" || len(request.ContractHash) != 64 {
			return result, errors.New("execution budget requires run ID and contract hash")
		}
		if request.ExecutionBudget.MaxInputTokens < 0 ||
			request.ExecutionBudget.MaxOutputTokens < 0 ||
			request.ExecutionBudget.MaxTotalTokens < 0 ||
			request.ExecutionBudget.MaxCostMicros < 0 {
			return result, errors.New("execution budget limits must be non-negative")
		}
	}
	if request.TotalTokens == 0 {
		request.TotalTokens, err = usageAdd(request.InputTokens, request.OutputTokens)
		if err != nil {
			return
		}
	}
	hash := usageHashRequest(request)
	err = store.authority.Update(usageContext(ctx), func(tx *ateauthority.Tx) error {
		key := usageRequestKey(request.TeamID, request.RequestID)
		id, err := getRecord[string](tx, "usage-reservation-request", key)
		if err == nil {
			stored, err := getRecord[usageReservation](tx, "usage-reservation", id)
			if err != nil {
				return err
			}
			legacyMatches := stored.AgentID == request.AgentID && stored.RunID == request.RunID && stored.TurnID == request.TurnID &&
				stored.InputTokens == request.InputTokens && stored.OutputTokens == request.OutputTokens && stored.TotalTokens == request.TotalTokens && stored.CostMicros == request.CostMicros &&
				(stored.ModelProfile == "" || stored.ModelProfile == request.ModelProfile) && (stored.Provider == "" || stored.Provider == request.Provider) && (stored.Model == "" || stored.Model == request.Model)
			if (stored.RequestHash != "" && stored.RequestHash != hash) || (stored.RequestHash == "" && !legacyMatches) {
				return fmt.Errorf("%w: reservation request %q", orchestration.ErrIdempotencyConflict, request.RequestID)
			}
			result = stored.Reservation
			return nil
		}
		if !errors.Is(err, orchestration.ErrNotFound) {
			return err
		}
		budget, err := usageBudgetTx(tx, request.TeamID)
		if err != nil {
			return err
		}
		if usageWouldExceed(int64(budget.Limits.MaxInputTokens), int64(budget.InputTokens), int64(budget.ReservedInputTokens), int64(request.InputTokens)) ||
			usageWouldExceed(int64(budget.Limits.MaxOutputTokens), int64(budget.OutputTokens), int64(budget.ReservedOutputTokens), int64(request.OutputTokens)) ||
			usageWouldExceed(int64(budget.Limits.MaxTotalTokens), int64(budget.TotalTokens), int64(budget.ReservedTotalTokens), int64(request.TotalTokens)) ||
			usageWouldExceed(budget.Limits.MaxCostMicros, budget.CostMicros, budget.ReservedCostMicros, request.CostMicros) {
			return tokenusage.ErrBudgetExceeded
		}
		if err := usageExecutionBudgetTx(tx, request); err != nil {
			return err
		}
		if err := usageCheckReferences(tx, request.TeamID, request.AgentID); err != nil {
			return err
		}
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return err
		}
		now := time.Now().UTC()
		result = tokenusage.Reservation{ID: "reservation-" + hex.EncodeToString(random[:]), TeamID: request.TeamID,
			AgentID: request.AgentID, RunID: request.RunID, TurnID: request.TurnID, RequestID: request.RequestID,
			InputTokens: request.InputTokens, OutputTokens: request.OutputTokens, TotalTokens: request.TotalTokens,
			CostMicros: request.CostMicros, RequestHash: hash, State: tokenusage.ReservationPending, CreatedAt: now, UpdatedAt: now}
		stored := usageReservation{Reservation: result, ModelProfile: request.ModelProfile, Provider: request.Provider, Model: request.Model, LifecycleVersion: 1}
		if err := createRecord(tx, "usage-reservation", result.ID, stored); err != nil {
			return err
		}
		return createRecord(tx, "usage-reservation-request", key, result.ID)
	})
	return
}

func executionBudgetZero(limits tokenusage.BudgetLimits) bool {
	return limits.MaxInputTokens == 0 &&
		limits.MaxOutputTokens == 0 &&
		limits.MaxTotalTokens == 0 &&
		limits.MaxCostMicros == 0
}

func usageExecutionBudgetTx(tx *ateauthority.Tx, request tokenusage.ReservationRequest) error {
	limits := request.ExecutionBudget
	if request.RunID == "" || executionBudgetZero(limits) {
		return nil
	}
	var input, output, total int
	var cost int64
	records, err := listRecords[tokenusage.Record](tx, "usage-record")
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.RunID != request.RunID {
			continue
		}
		input += record.Usage.Input
		output += record.Usage.Output
		total += record.Usage.Total
		cost += record.CostMicros
	}
	reservations, err := listRecords[usageReservation](tx, "usage-reservation")
	if err != nil {
		return err
	}
	for _, reservation := range reservations {
		if reservation.RunID != request.RunID || reservation.State != tokenusage.ReservationPending {
			continue
		}
		input += reservation.InputTokens
		output += reservation.OutputTokens
		total += reservation.TotalTokens
		cost += reservation.CostMicros
	}
	if usageWouldExceed(int64(limits.MaxInputTokens), int64(input), 0, int64(request.InputTokens)) ||
		usageWouldExceed(int64(limits.MaxOutputTokens), int64(output), 0, int64(request.OutputTokens)) ||
		usageWouldExceed(int64(limits.MaxTotalTokens), int64(total), 0, int64(request.TotalTokens)) ||
		usageWouldExceed(limits.MaxCostMicros, cost, 0, request.CostMicros) {
		return tokenusage.ErrBudgetExceeded
	}
	return nil
}

func (store *Store) SettleReservation(ctx context.Context, id string, record tokenusage.Record) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("reservation ID is required")
	}
	record, err := usageNormalizeReceipt(record)
	if err != nil {
		return err
	}
	return store.authority.Update(usageContext(ctx), func(tx *ateauthority.Tx) error {
		stored, err := getRecord[usageReservation](tx, "usage-reservation", id)
		if err != nil {
			return err
		}
		// Direct settlement retains the legacy owner/request policy. Staging
		// and recovery additionally bind run, turn and model identity.
		if stored.TeamID != record.TeamID || stored.AgentID != record.AgentID || stored.RequestID != record.RequestID {
			return fmt.Errorf("%w: reservation owner or request mismatch", orchestration.ErrInvalidEvent)
		}
		if len(stored.Outcome) > 0 {
			var staged tokenusage.Record
			if err := json.Unmarshal(stored.Outcome, &staged); err != nil {
				return err
			}
			if !usageRecordMatches(staged, record) {
				return fmt.Errorf("%w: staged settlement differs", orchestration.ErrIdempotencyConflict)
			}
		}
		existing, err := usageExistingRecord(tx, record.TeamID, record.RequestID)
		if err == nil {
			if !usageRecordMatches(existing, record) {
				return fmt.Errorf("%w: settlement request %q", orchestration.ErrIdempotencyConflict, record.RequestID)
			}
		} else if !errors.Is(err, orchestration.ErrNotFound) {
			return err
		}
		if stored.State == tokenusage.ReservationSettled {
			return nil
		}
		if stored.State == tokenusage.ReservationReleased {
			return fmt.Errorf("%w: reservation was released", orchestration.ErrInvalidEvent)
		}
		if stored.State != tokenusage.ReservationPending {
			return fmt.Errorf("%w: reservation is not pending", orchestration.ErrSequenceConflict)
		}
		if err := usageRecordTx(tx, record); err != nil {
			return err
		}
		stored.State, stored.UpdatedAt = tokenusage.ReservationSettled, time.Now().UTC()
		return putRecord(tx, "usage-reservation", id, stored)
	})
}

func (store *Store) ReleaseReservation(ctx context.Context, id, reason string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("reservation ID is required")
	}
	return store.authority.Update(usageContext(ctx), func(tx *ateauthority.Tx) error {
		stored, err := getRecord[usageReservation](tx, "usage-reservation", id)
		if err != nil {
			return err
		}
		if stored.State == tokenusage.ReservationSettled || stored.State == tokenusage.ReservationReleased {
			return nil
		}
		if stored.State != tokenusage.ReservationPending || stored.LifecycleVersion != 1 || stored.Dispatched || len(stored.Outcome) > 0 {
			return fmt.Errorf("%w: cannot release reservation (%s)", orchestration.ErrInvalidEvent, reason)
		}
		stored.State, stored.UpdatedAt = tokenusage.ReservationReleased, time.Now().UTC()
		return putRecord(tx, "usage-reservation", id, stored)
	})
}

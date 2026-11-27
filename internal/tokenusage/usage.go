// Package tokenusage provides a provider-neutral view of model token usage.
package tokenusage

import (
	"context"
	"errors"
	"maps"
	"math"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

// Snapshot is the token usage reported for one model response. Output includes
// Reasoning: the latter is a completion-token detail, not an additional total.
type Snapshot struct {
	Input     int
	Cached    int
	Output    int
	Reasoning int
	Total     int
}

var ErrBudgetExceeded = errors.New("token budget exceeded")

type Record struct {
	RecordID     string
	TeamID       orchestration.TeamID
	AgentID      orchestration.AgentID
	RunID        orchestration.RunID
	TurnID       string
	RequestID    string
	ModelProfile string
	Provider     string
	Model        string
	Usage        Snapshot
	CostMicros   int64
	Quality      string
	RecordedAt   time.Time
}

type ReservationState string

const (
	ReservationPending  ReservationState = "pending"
	ReservationSettled  ReservationState = "settled"
	ReservationReleased ReservationState = "released"
)

type ReservationRequest struct {
	TeamID          orchestration.TeamID
	AgentID         orchestration.AgentID
	RunID           orchestration.RunID
	TurnID          string
	RequestID       string
	ContractHash    string
	ExecutionBudget BudgetLimits
	InputTokens     int
	OutputTokens    int
	TotalTokens     int
	CostMicros      int64
	ModelProfile    string
	Provider        string
	Model           string
}

type Reservation struct {
	ID           string
	TeamID       orchestration.TeamID
	AgentID      orchestration.AgentID
	RunID        orchestration.RunID
	TurnID       string
	RequestID    string
	InputTokens  int
	OutputTokens int
	TotalTokens  int
	CostMicros   int64
	RequestHash  string
	State        ReservationState
	CreatedAt    time.Time
	UpdatedAt    time.Time
	Dispatched   bool
}

// UsageQuery selects immutable ledger rows for reporting. A zero time bound
// means unbounded; an empty optional field means all values.
type UsageQuery struct {
	TeamID   orchestration.TeamID
	AgentID  orchestration.AgentID
	Provider string
	Model    string
	Since    time.Time
	Until    time.Time
}

type UsageAggregate struct {
	Records         int
	InputTokens     int
	CachedTokens    int
	OutputTokens    int
	ReasoningTokens int
	TotalTokens     int
	CostMicros      int64
}

type UsageReport struct {
	Query     UsageQuery
	Aggregate UsageAggregate
}

type BudgetLimits struct {
	MaxInputTokens  int
	MaxOutputTokens int
	MaxTotalTokens  int
	MaxCostMicros   int64
}

type BudgetSnapshot struct {
	TeamID               orchestration.TeamID
	Limits               BudgetLimits
	InputTokens          int
	OutputTokens         int
	TotalTokens          int
	CostMicros           int64
	ReservedInputTokens  int
	ReservedOutputTokens int
	ReservedTotalTokens  int
	ReservedCostMicros   int64
	Exhausted            bool
	UpdatedAt            time.Time
}

// Ledger is the narrow runtime contract for durable usage accounting. A nil
// ledger means accounting is disabled; model execution must remain functional.
type Ledger interface {
	// RecordUsage records token usage against its operation and execution scope.
	RecordUsage(context.Context, Record) error
	CanAdmit(context.Context, orchestration.TeamID) (bool, error)
}

// ReservationLedger extends Ledger with durable admission reservations. The
// runtime uses it when available and falls back to RecordUsage for legacy
// stores.
type ReservationLedger interface {
	Ledger
	Reserve(context.Context, ReservationRequest) (Reservation, error)
	SettleReservation(context.Context, string, Record) error
	ReleaseReservation(context.Context, string, string) error
}

// DurableReservationLedger journals the last boundary before dispatch and the
// receipt before settlement. Recovery requires exclusive runtime ownership and
// MUST NOT run concurrently with active model requests (it is not a TTL reaper).
type DurableReservationLedger interface {
	ReservationLedger
	MarkDispatched(context.Context, string) error
	StageSettlement(context.Context, string, Record) error
	RecoverReservations(context.Context) (int, error)
	ModelRequestSequence(context.Context, orchestration.TeamID, string) (uint64, error)
}

// Reader is implemented by durable ledgers that expose reporting without
// coupling the runtime to a database-specific query API.
type Reader interface {
	// UsageReport aggregates recorded usage matching the supplied query.
	UsageReport(context.Context, UsageQuery) (UsageReport, error)
}

// Price defines provider billing units in microcurrency per one million
// tokens. Cached input is charged independently when the provider exposes it.
// A nonzero reasoning rate replaces the output rate for the reasoning subset;
// zero uses the ordinary output rate for all completion tokens.
type Price struct {
	InputMicrosPerMillion       int64
	CachedInputMicrosPerMillion int64
	OutputMicrosPerMillion      int64
	ReasoningMicrosPerMillion   int64
}

// Pricer calculates cost only when a matching, explicitly configured price
// exists. Unknown models remain unpriced instead of using a guessed tariff.
type Pricer interface {
	// CostMicros prices the usage snapshot for a provider and model and reports whether a
	// price is available.
	CostMicros(provider, model string, usage Snapshot) (int64, bool)
}

// ReservationPricer bounds cost before the provider reports how its output
// allowance is partitioned into visible output and reasoning. Settlement must
// always use CostMicros with the actual usage, never this worst-case bound.
type ReservationPricer interface {
	Pricer
	ReservationCostMicros(provider, model string, usage Snapshot) (int64, bool)
}

type PriceTable struct {
	prices map[string]Price
}

func NewPriceTable(prices map[string]Price) PriceTable {
	copyPrices := make(map[string]Price, len(prices))
	maps.Copy(copyPrices, prices)
	return PriceTable{prices: copyPrices}
}

func (table PriceTable) CostMicros(provider, model string, usage Snapshot) (int64, bool) {
	price, ok := table.price(provider, model)
	if !ok {
		return 0, false
	}
	return costForPrice(price, usage)
}

func (table PriceTable) price(provider, model string) (Price, bool) {
	price, ok := table.prices[provider+"/"+model]
	if !ok {
		price, ok = table.prices[model]
	}
	return price, ok
}

func costForPrice(price Price, usage Snapshot) (int64, bool) {
	if usage.Input < 0 || usage.Cached < 0 || usage.Output < 0 || usage.Reasoning < 0 || usage.Cached > usage.Input || usage.Reasoning > usage.Output {
		return 0, false
	}
	output, reasoning := usage.Output, 0
	if price.ReasoningMicrosPerMillion != 0 {
		output -= usage.Reasoning
		reasoning = usage.Reasoning
	}
	cost := int64(0)
	for _, component := range []struct {
		tokens int
		rate   int64
	}{
		{usage.Input - usage.Cached, price.InputMicrosPerMillion},
		{usage.Cached, price.CachedInputMicrosPerMillion},
		{output, price.OutputMicrosPerMillion},
		{reasoning, price.ReasoningMicrosPerMillion},
	} {
		if component.rate < 0 || (component.tokens > 0 && component.rate > (math.MaxInt64-cost)/int64(component.tokens)) {
			return 0, false
		}
		cost += int64(component.tokens) * component.rate
	}
	return cost / 1_000_000, true
}

func (table PriceTable) ReservationCostMicros(provider, model string, usage Snapshot) (int64, bool) {
	price, ok := table.price(provider, model)
	if !ok || price.InputMicrosPerMillion < 0 || price.CachedInputMicrosPerMillion < 0 || price.OutputMicrosPerMillion < 0 || price.ReasoningMicrosPerMillion < 0 {
		return 0, false
	}
	// Cache misses and the more expensive completion category provide the
	// conservative charge when neither detail is known before dispatch.
	price.InputMicrosPerMillion = max(price.InputMicrosPerMillion, price.CachedInputMicrosPerMillion)
	price.OutputMicrosPerMillion = max(price.OutputMicrosPerMillion, price.ReasoningMicrosPerMillion)
	price.ReasoningMicrosPerMillion = 0
	usage.Cached, usage.Reasoning = 0, 0
	return costForPrice(price, usage)
}

// FromMessage extracts normalized usage from an Eino assistant message.
func FromMessage(message *schema.Message) Snapshot {
	if message == nil || message.ResponseMeta == nil || message.ResponseMeta.Usage == nil {
		return Snapshot{}
	}
	usage := message.ResponseMeta.Usage
	return Snapshot{
		Input:     usage.PromptTokens,
		Cached:    usage.PromptTokenDetails.CachedTokens,
		Output:    usage.CompletionTokens,
		Reasoning: usage.CompletionTokensDetails.ReasoningTokens,
		Total:     usage.TotalTokens,
	}
}

// CacheHitPercent returns the provider-reported cached share of input tokens.
func (usage Snapshot) CacheHitPercent() float64 {
	if usage.Input <= 0 || usage.Cached <= 0 {
		return 0
	}
	percent := float64(usage.Cached) / float64(usage.Input) * 100
	if percent > 100 {
		return 100
	}
	return percent
}

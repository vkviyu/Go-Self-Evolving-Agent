package tokenusage

import (
	"math"
	"testing"
)

func TestReasoningPricePartitionsCompletionTokens(t *testing.T) {
	usage := Snapshot{Input: 10, Cached: 2, Output: 10, Reasoning: 4}
	price := Price{InputMicrosPerMillion: 1_000_000, CachedInputMicrosPerMillion: 500_000, OutputMicrosPerMillion: 2_000_000, ReasoningMicrosPerMillion: 3_000_000}
	table := NewPriceTable(map[string]Price{"provider/model": price})
	if cost, ok := table.CostMicros("provider", "model", usage); !ok || cost != 33 {
		t.Fatalf("partitioned cost = %d, priced=%v; want 33", cost, ok)
	}
	price.ReasoningMicrosPerMillion = 0
	table = NewPriceTable(map[string]Price{"provider/model": price})
	if cost, ok := table.CostMicros("provider", "model", usage); !ok || cost != 29 {
		t.Fatalf("normal output cost = %d, priced=%v; want 29", cost, ok)
	}
}

func TestPricingRejectsOverflowAndInvalidComponents(t *testing.T) {
	for _, tc := range []struct {
		price Price
		usage Snapshot
	}{
		{Price{InputMicrosPerMillion: math.MaxInt64}, Snapshot{Input: 2}},
		{Price{OutputMicrosPerMillion: -1}, Snapshot{}},
		{Price{}, Snapshot{Input: 1, Cached: 2}},
		{Price{}, Snapshot{Output: 1, Reasoning: 2}},
	} {
		if cost, ok := NewPriceTable(map[string]Price{"m": tc.price}).CostMicros("p", "m", tc.usage); ok || cost != 0 {
			t.Fatalf("accepted invalid/overflowing charge: %+v => %d", tc, cost)
		}
	}
}

func TestReservationPricesUnknownReasoningAtWorstCaseRate(t *testing.T) {
	table := NewPriceTable(map[string]Price{"p/m": {
		InputMicrosPerMillion: 1_000_000, CachedInputMicrosPerMillion: 500_000,
		OutputMicrosPerMillion: 2_000_000, ReasoningMicrosPerMillion: 7_000_000,
	}})
	usage := Snapshot{Input: 10, Output: 10}
	if cost, ok := table.ReservationCostMicros("p", "m", usage); !ok || cost != 80 {
		t.Fatalf("worst-case reservation = %d priced=%v; want 80", cost, ok)
	}
	if cost, ok := table.CostMicros("p", "m", usage); !ok || cost != 30 {
		t.Fatalf("actual non-reasoning charge = %d priced=%v; want 30", cost, ok)
	}
}

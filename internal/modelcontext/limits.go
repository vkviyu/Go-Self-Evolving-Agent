// Package modelcontext owns model capacity metadata and context budgets. It
// has no provider transport, tokenizer, session, or UI dependencies.
package modelcontext

import (
	"fmt"
	"regexp"
	"strings"
)

type Source string

const (
	SourceConfigured Source = "configured"
	SourceProvider   Source = "provider"
	SourceCatalog    Source = "catalog"
	SourceFallback   Source = "fallback"
	FallbackWindow          = 32768
)

type Metadata struct {
	Window          int    `json:"window"`
	Source          Source `json:"source"`
	MaxOutputTokens int    `json:"max_output_tokens"` // Zero means no declared cap.
	OutputSource    Source `json:"output_source"`     // A provider may declare one field only.
}

// Informer is an optional model capability; runtime code need not know how a
// provider discovered its limits. Capacity and host policy remain distinct.
type Informer interface{ ModelContext() Metadata }

// Overrides contains host policy. Zero selects the automatic/default value.
type Overrides struct{ Window, Output, Trigger int }

type Limits struct {
	Window        int
	WindowSource  Source
	OutputReserve int
	InputBudget   int      // Complete input, including instructions and tool schemas.
	Trigger       int      // Soft threshold on complete input, never above InputBudget.
	Capacity      Metadata // Discovered values, not rewritten by host policy overrides.
}

// Published capacities, checked against the linked official model pages on
// 2026-10-16. Match exact IDs and dated snapshots, never arbitrary prefixes or
// private deployment names. A gateway may advertise a smaller effective window.
// https://developers.openai.com/api/docs/models/gpt-5.6-sol
// https://developers.openai.com/api/docs/models/gpt-6.1-sol
// https://developers.openai.com/api/docs/models
// https://developers.openai.com/api/docs/models/gpt-5.2
// https://developers.openai.com/api/docs/models/gpt-5-codex
// https://developers.openai.com/api/docs/models/gpt-4.1
// https://developers.openai.com/api/docs/models/gpt-4o-mini
var catalog = map[string]struct{ window, output int }{
	"gpt-5.6-sol": {1050000, 128000}, "gpt-5.6": {1050000, 128000},
	"gpt-5.6-terra": {window: 1050000}, "gpt-5.6-luna": {window: 1050000},
	"gpt-6.1-sol": {window: 1050000}, "gpt-6-sol": {window: 1050000}, "gpt-6-astra": {window: 1050000}, "gpt-6-luna": {window: 1050000},
	"gpt-5.2": {400000, 128000}, "gpt-5.2-pro": {window: 400000}, "gpt-5-codex": {window: 400000},
	"gpt-4.1": {1047576, 32768}, "gpt-4o": {window: 128000}, "gpt-4o-mini": {window: 128000},
}

var datedSnapshot = regexp.MustCompile(`-\d{4}-\d{2}-\d{2}$`)

func Lookup(provider, name string) Metadata {
	if strings.EqualFold(strings.TrimSpace(provider), "openai") {
		name = datedSnapshot.ReplaceAllString(strings.TrimSpace(name), "")
		if capacity, ok := catalog[name]; ok {
			metadata := Metadata{Window: capacity.window, Source: SourceCatalog}
			if capacity.output > 0 {
				metadata.MaxOutputTokens, metadata.OutputSource = capacity.output, SourceCatalog
			}
			return metadata
		}
	}
	return Metadata{Window: FallbackWindow, Source: SourceFallback}
}

// Resolve combines deployment capacity with host policy. A provider-declared
// window is a hard limit; catalog entries and fallback values are defaults that
// an operator can override for a differently configured deployment.
func Resolve(capacity Metadata, overrides Overrides) (Limits, error) {
	if overrides.Window < 0 || overrides.Output < 0 || overrides.Trigger < 0 {
		return Limits{}, fmt.Errorf("context window, output allowance, and compaction trigger must not be negative")
	}
	if capacity.Window <= 1 {
		capacity.Window, capacity.Source = FallbackWindow, SourceFallback
	}
	window, source := capacity.Window, capacity.Source
	if overrides.Window > 0 {
		if capacity.Source == SourceProvider && overrides.Window > capacity.Window {
			return Limits{}, fmt.Errorf("configured context window (%d) exceeds provider-declared context window (%d)", overrides.Window, capacity.Window)
		}
		window, source = overrides.Window, SourceConfigured
	}
	output := overrides.Output
	if output == 0 {
		output = max(1, min(8192, window/4))
	}
	if capacity.MaxOutputTokens > 0 {
		output = min(output, capacity.MaxOutputTokens)
	}
	budget := InputLimit(window, output)
	trigger := overrides.Trigger
	if trigger == 0 {
		trigger = DefaultTrigger(window, output)
	}
	trigger = min(trigger, budget)
	if err := Validate(window, trigger, output); err != nil {
		return Limits{}, err
	}
	return Limits{
		Window: window, WindowSource: source, OutputReserve: output,
		InputBudget: budget, Trigger: trigger, Capacity: capacity,
	}, nil
}

// InputLimit leaves room for generated output/reasoning and estimation error.
func InputLimit(window, output int) int {
	if window <= 1 {
		return 1
	}
	remaining := window - SafetyMargin(window)
	if output >= remaining {
		return 1
	}
	return max(1, remaining-max(0, output))
}

func SafetyMargin(window int) int { return max(1, min(4096, window/20)) }

// DefaultTrigger separates model capacity from the working-context policy.
// Large windows are valid, but a 200k working set avoids retaining a million
// tokens of stale tool output by default. Users can override this soft limit.
func DefaultTrigger(window, output int) int {
	// Divide before multiplying so even a maximum-sized configured int is safe.
	workingSet := window/5*4 + window%5*4/5
	return max(1, min(workingSet, 200000, InputLimit(window, output)))
}

func Validate(window, trigger, output int) error {
	if window <= 1 || output <= 0 || output >= window {
		return fmt.Errorf("context window (%d) must exceed the output allowance (%d)", window, output)
	}
	if output >= window-SafetyMargin(window) {
		return fmt.Errorf("context window (%d) must leave input space after output allowance (%d) and safety margin (%d)", window, output, SafetyMargin(window))
	}
	if trigger <= 0 || trigger >= window {
		return fmt.Errorf("compaction trigger (%d) must be positive and below context window (%d)", trigger, window)
	}
	return nil
}

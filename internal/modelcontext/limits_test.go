package modelcontext

import (
	"strings"
	"testing"
)

func TestCatalogDoesNotGuessPrivateAliasesOrDifferentProviders(t *testing.T) {
	for _, test := range []struct {
		provider, name string
		window         int
		source         Source
	}{
		{"openai", "gpt-5.6-sol", 1050000, SourceCatalog},
		{"OPENAI", "gpt-5.2-2025-12-11", 400000, SourceCatalog},
		{"openai", "gpt-5.6-sol-private", FallbackWindow, SourceFallback},
		{"openai", "private-gpt-5.6-sol", FallbackWindow, SourceFallback},
		{"ollama", "gpt-5.6-sol", FallbackWindow, SourceFallback},
	} {
		if got := Lookup(test.provider, test.name); got.Window != test.window || got.Source != test.source {
			t.Errorf("%s/%s: %+v", test.provider, test.name, got)
		}
	}
}

func TestWorkingThresholdReservesOutputAndSeparatesLargeCapacity(t *testing.T) {
	for _, test := range []struct{ window, output, trigger int }{
		{32768, 8192, 22938}, {128000, 8192, 102400}, {1050000, 8192, 200000}, {16000, 6000, 9200},
	} {
		got := DefaultTrigger(test.window, test.output)
		if got != test.trigger || got > InputLimit(test.window, test.output) || got+test.output >= test.window {
			t.Errorf("window=%d output=%d trigger=%d want=%d", test.window, test.output, got, test.trigger)
		}
	}
	if err := Validate(8192, 6000, 8192); err == nil {
		t.Fatal("output allowance consuming the entire context accepted")
	}
}

func TestResolveKeepsHostPolicySeparateFromCapacity(t *testing.T) {
	provider := Metadata{Window: 128000, Source: SourceProvider, MaxOutputTokens: 4096, OutputSource: SourceProvider}
	if _, err := Resolve(provider, Overrides{Window: 1000000}); err == nil || !strings.Contains(err.Error(), "provider-declared") {
		t.Fatalf("exceeding provider capacity: %v", err)
	}
	limits, err := Resolve(provider, Overrides{Window: 64000, Output: 8192, Trigger: 63000})
	if err != nil {
		t.Fatal(err)
	}
	if limits.Window != 64000 || limits.WindowSource != SourceConfigured || limits.Capacity != provider || limits.OutputReserve != 4096 || limits.InputBudget != 56704 || limits.Trigger != limits.InputBudget {
		t.Fatalf("limits=%+v", limits)
	}
	for _, source := range []Source{SourceCatalog, SourceFallback} {
		limits, err := Resolve(Metadata{Window: 32768, Source: source}, Overrides{Window: 1000000})
		if err != nil || limits.Window != 1000000 || limits.WindowSource != SourceConfigured || limits.Capacity.Source != source {
			t.Fatalf("override source=%s: limits=%+v err=%v", source, limits, err)
		}
	}
}

func TestResolveOutputOnlyMetadataAndAutomaticDefaults(t *testing.T) {
	for _, test := range []struct {
		name     string
		capacity Metadata
		override Overrides
		window   int
		output   int
	}{
		{"unknown", Metadata{}, Overrides{}, FallbackWindow, 8192},
		{"known catalog", Lookup("openai", "gpt-5.6-sol"), Overrides{}, 1050000, 8192},
		{"default limited by output cap", Metadata{MaxOutputTokens: 1024, OutputSource: SourceProvider}, Overrides{}, FallbackWindow, 1024},
		{"explicit limited by output cap", Metadata{MaxOutputTokens: 1024, OutputSource: SourceProvider}, Overrides{Output: 2048}, FallbackWindow, 1024},
		{"host output lower than cap", Metadata{MaxOutputTokens: 1024, OutputSource: SourceProvider}, Overrides{Output: 512}, FallbackWindow, 512},
		{"small window default", Metadata{Window: 1000, Source: SourceProvider}, Overrides{}, 1000, 250},
	} {
		t.Run(test.name, func(t *testing.T) {
			limits, err := Resolve(test.capacity, test.override)
			if err != nil {
				t.Fatal(err)
			}
			if limits.Window != test.window || limits.OutputReserve != test.output || limits.InputBudget != limits.Window-limits.OutputReserve-SafetyMargin(limits.Window) || limits.Trigger > limits.InputBudget {
				t.Fatalf("limits=%+v", limits)
			}
			if test.capacity.Window == 0 && limits.Capacity.Source != SourceFallback {
				t.Fatalf("unknown window presented as known capacity: %+v", limits)
			}
			if limits.Capacity.MaxOutputTokens != test.capacity.MaxOutputTokens || limits.Capacity.OutputSource != test.capacity.OutputSource {
				t.Fatalf("output-only metadata lost: %+v", limits.Capacity)
			}
		})
	}
}

func TestResolveRejectsInsufficientInputSpaceAndNegativeOverrides(t *testing.T) {
	for _, overrides := range []Overrides{
		{Window: -1}, {Output: -1}, {Trigger: -1}, {Window: 1}, {Window: 2, Output: 1},
		{Window: 1000, Output: 950}, {Window: 1000, Output: 1001},
	} {
		if _, err := Resolve(Metadata{}, overrides); err == nil {
			t.Errorf("accepted invalid override: %+v", overrides)
		}
	}
	if err := Validate(1000, 500, 950); err == nil {
		t.Fatal("accepted output that leaves no input space after safety margin")
	}
}

func TestContextBudgetArithmeticDoesNotOverflow(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	limits, err := Resolve(Metadata{}, Overrides{Window: maxInt, Output: 8192, Trigger: maxInt})
	if err != nil || limits.InputBudget != maxInt-8192-4096 || limits.Trigger != limits.InputBudget {
		t.Fatalf("large capacity: limits=%+v err=%v", limits, err)
	}
	if got := DefaultTrigger(maxInt, 8192); got != 200000 {
		t.Fatalf("overflowed automatic trigger: %d", got)
	}
	if got := InputLimit(1000, maxInt); got != 1 {
		t.Fatalf("overflowed input limit: %d", got)
	}
	if got := InputLimit(-maxInt, maxInt); got != 1 {
		t.Fatalf("overflowed invalid capacity: %d", got)
	}
}

package agentruntime

import (
	"sync"

	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/modelcontext"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenizer"
)

type ContextState string

const (
	ContextReady             ContextState = "ready"
	ContextCompactionPending ContextState = "compaction_pending"
	ContextOverBudget        ContextState = "over_budget"
)

// ContextUsage separates complete input size, working policy, and discovered
// model capacity. Pending is an assessment, not a queued background model call.
type ContextUsage struct {
	Used            int                   `json:"used"`
	Window          int                   `json:"window"`
	Trigger         int                   `json:"trigger"`
	Status          tokenizer.Status      `json:"status"`
	Model           string                `json:"model"`
	Encoding        string                `json:"encoding"`
	ProviderInput   int                   `json:"provider_input"`
	WindowSource    modelcontext.Source   `json:"window_source"`
	InputLimit      int                   `json:"input_limit"`
	OutputReserve   int                   `json:"output_reserve"`
	Capacity        modelcontext.Metadata `json:"capacity"`
	State           ContextState          `json:"state"`
	NeedsCompaction bool                  `json:"needs_compaction"`
}

type contextCalibration struct {
	mutex         sync.RWMutex
	providerInput int
	observed      map[string]struct{}
}

// ContextUsage counts the supplied conversation using the configured model
// tokenizer. ProviderInput is the last authoritative prompt count, when the
// provider returned one; it is useful to explain calibration in the UI.
func (f *Factory) ContextUsage(messages []*schema.Message) ContextUsage {
	input := append([]*schema.Message{schema.SystemMessage(f.runtimeInstruction())}, messages...)
	return f.contextUsage(input, f.contextToolSchemas(), f.config.MaxOutputTokens)
}

// contextUsage accepts an already complete prompt: instructions and tools must
// be counted exactly once, including when assessing the last dispatched input.
func (f *Factory) contextUsage(input []*schema.Message, tools []contextToolSchema, output int) ContextUsage {
	providerInput := 0
	if f.calibration != nil {
		f.calibration.mutex.RLock()
		providerInput = f.calibration.providerInput
		f.calibration.mutex.RUnlock()
	}
	usage := ContextUsage{
		Used:          f.countContextInput(input, tools),
		Window:        f.config.ContextWindowTokens,
		Trigger:       f.config.CompactionTriggerTokens,
		Status:        f.tokenizer.Status(),
		Model:         f.tokenizer.Model(),
		Encoding:      f.tokenizer.Encoding(),
		ProviderInput: providerInput,
		WindowSource:  f.config.ContextWindowSource,
		InputLimit:    modelcontext.InputLimit(f.config.ContextWindowTokens, output),
		OutputReserve: output,
		Capacity:      f.capacity,
		State:         ContextReady,
	}
	usage.Trigger = min(usage.Trigger, usage.InputLimit)
	usage.NeedsCompaction = usage.Trigger > 0 && usage.Used >= usage.Trigger
	if usage.NeedsCompaction {
		usage.State = ContextCompactionPending
	}
	if usage.Used > usage.InputLimit {
		usage.State, usage.NeedsCompaction = ContextOverBudget, true
	}
	return usage
}

func (f *Factory) observeProviderInput(requestID string, actual, estimated int) {
	if f.calibration != nil {
		f.calibration.mutex.Lock()
		defer f.calibration.mutex.Unlock()
		if requestID != "" {
			if _, exists := f.calibration.observed[requestID]; exists {
				return
			}
			if f.calibration.observed == nil {
				f.calibration.observed = make(map[string]struct{})
			}
			f.calibration.observed[requestID] = struct{}{}
		}
		f.tokenizer.ObserveProviderInput(actual, estimated)
		f.calibration.providerInput = actual
		return
	}
	f.tokenizer.ObserveProviderInput(actual, estimated)
}

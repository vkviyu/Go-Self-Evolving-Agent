package evolutiontools

import (
	"encoding/json"

	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

// StrategyBindings cannot register suites or alter host-owned expectations.
func StrategyBindings(store StrategyStore, caller ResolveStrategyCaller) []tool.Binding {
	adapter := strategyAdapter{store: store, caller: caller}
	specSchema := json.RawMessage(`{"type":"object","properties":{"version":{"type":"integer"},"revision":{"type":"string"},"instruction_suffix":{"type":"string"},"max_iterations":{"type":"integer"},"max_output_tokens":{"type":"integer"}},"required":["version","revision","instruction_suffix"],"additionalProperties":false}`)
	idSchema := json.RawMessage(`{"type":"object","properties":{"candidate_id":{"type":"string"},"suite_id":{"type":"string"},"report_id":{"type":"string"}},"additionalProperties":false}`)
	return []tool.Binding{
		{
			Descriptor: tool.Descriptor{
				Name:        "strategy_propose",
				Revision:    "builtin-v1",
				RiskClass:   tool.RiskWrite,
				InputSchema: specSchema,
				Description: "Propose a version:1 LoopSpec for this root session. Only instruction_suffix and lower iteration/output caps; no model, tools, permissions or execution gate changes. Requires approval and never activates. New revision is required for repairs.",
			},
			Executor: tool.InvokeFunc(adapter.propose),
		},
		{
			Descriptor: tool.Descriptor{
				Name:        "strategy_inspect",
				Revision:    "builtin-v1",
				RiskClass:   tool.RiskRead,
				InputSchema: idSchema,
				Description: "Inspect own candidate_id or report_id. Cannot edit host-owned shadow expectations. Reports prove only non-regression on the named tool-free suite, not general improvement.",
			},
			Executor: tool.InvokeFunc(adapter.inspect),
		},
		{
			Descriptor: tool.Descriptor{
				Name:        "strategy_shadow",
				Revision:    "builtin-v1",
				RiskClass:   tool.RiskExecute,
				InputSchema: idSchema,
				Description: "Compare candidate_id with current baseline against user-registered suite_id. Requires approval for model requests, at most 2 minutes, 8 fixtures, no executable tools, charged to current Team. Cannot create/alter test expectations. Inspect failures and repair through the same Eino loop.",
			},
			Executor: tool.InvokeFunc(adapter.shadow),
		},
		{
			Descriptor: tool.Descriptor{
				Name:        "strategy_publish",
				Revision:    "builtin-v1",
				RiskClass:   tool.RiskWrite,
				InputSchema: idSchema,
				Description: "Request separate approval to promote exact passing report_id for this root session's baseline config. Old Turns remain pinned; only future Turns change. Execution failure restores baseline for future Turns, never undoes effects or replays old work.",
			},
			Executor: tool.InvokeFunc(adapter.publish),
		},
	}
}

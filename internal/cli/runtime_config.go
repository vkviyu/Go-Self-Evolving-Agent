package cli

import (
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/modelgateway"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/strategy"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

// Chat and explicit shadow verification must identify exactly the same base
// execution config; neither path invents a second set of runtime defaults.
func rootRuntimeConfig(options chatOptions, actor orchestration.ActorScope, ledger tokenusage.Ledger) agentruntime.Config {
	return agentruntime.Config{
		StrategyScope: strategy.Digest(options.model),
		Name:          "local_agent", Description: "A local-first coding assistant with policy-controlled tools.",
		Instruction: options.instruction, MaxIterations: options.maxIterations, MaxOutputTokens: options.maxOutputTokens,
		ContextWindowTokens: options.contextWindowTokens, CompactionTriggerTokens: options.compactionTriggerTokens,
		Tokenizer: modelgateway.NewTokenizer(options.model), ActorScope: actor, ModelProfile: options.model.Model,
		Provider: string(options.model.Provider), Model: options.model.Model, UsageLedger: ledger,
	}
}

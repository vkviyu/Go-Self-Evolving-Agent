package agentruntime

import (
	"context"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/modelcontext"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/strategy"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenizer"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

// Config controls stable Agent behavior independent of any model provider.
type Config struct {
	Name                    string
	Description             string
	Instruction             string
	MaxIterations           int
	MaxOutputTokens         int
	ContextWindowTokens     int
	CompactionTriggerTokens int
	ContextWindowSource     modelcontext.Source
	Tokenizer               tokenizer.Counter
	ActorScope              orchestration.ActorScope
	ModelProfile            string
	Provider                string
	Model                   string
	UsageLedger             tokenusage.Ledger
	Pricing                 tokenusage.Pricer
	GoalMode                agentgoal.Config
	GoalStore               agentgoal.Store
	GoalBudget              agentgoal.BudgetReader
	// GoalEvaluator is the independent assessment path. Self-assessment is
	// always derived from the goal_report tool and cannot be replaced here.
	GoalEvaluator agentgoal.Evaluator
	// ContextArtifacts receives oversized tool observations before they are
	// replaced with content-addressed references in model input.
	ContextArtifacts ContextArtifactStore
	ResolveModel     func(context.Context, string) (ModelBinding, error)
	Strategies       strategy.Store
	// StrategyScope binds shadow evidence to host/provider settings not present
	// in the legacy execution identity. It contains no credentials.
	StrategyScope string
}

type ContextArtifactStore interface {
	Put(context.Context, []byte) (string, error)
}

package agentruntime

import (
	"errors"

	"github.com/cloudwego/eino/components/model"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentprofile"
	"github.com/vkviyu/go-self-evolving-agent/internal/modelcontext"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenizer"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

var ErrTurnModelChanged = errors.New("turn model snapshot changed")

// Factory creates a new Eino runner for each turn. This is what freezes the
// tool registry generation for the entire ReAct loop.
type Factory struct {
	config           Config
	model            model.BaseChatModel
	registry         *tool.Registry
	pipeline         *tool.Pipeline
	events           session.EventStore
	checkpoints      session.CheckpointStore
	tokenizer        tokenizer.Counter
	calibration      *contextCalibration
	capacity         modelcontext.Metadata
	outputPolicy     int // Original host setting; zero remains automatic for a different model.
	goalMode         agentgoal.Config
	goalStore        agentgoal.Store
	goalBudget       agentgoal.BudgetReader
	goalEval         agentgoal.Evaluator
	artifacts        ContextArtifactStore
	profile          *agentprofile.Profile
	profileSession   session.ID
	strategyPinned   bool
	strategyID       string
	baselineConfigID string
	mailbox          BackgroundMailbox
}

// NewFactory validates reusable dependencies. Provider-specific model setup is
// intentionally outside this package.
func NewFactory(
	config Config,
	chatModel model.BaseChatModel,
	registry *tool.Registry,
	pipeline *tool.Pipeline,
	events session.EventStore,
	checkpoints session.CheckpointStore,
) (*Factory, error) {
	if chatModel == nil || registry == nil || pipeline == nil || events == nil || checkpoints == nil {
		return nil, errors.New("Agent factory requires model, registry, pipeline, event store and checkpoint store")
	}
	if config.Name == "" {
		config.Name = "local_agent"
	}
	if config.Description == "" {
		config.Description = "A local-first tool-using Agent."
	}
	if config.MaxIterations <= 0 {
		config.MaxIterations = 20
	}
	capacity := modelCapacity(chatModel, config.Provider, config.Model)
	outputPolicy := config.MaxOutputTokens
	limits, err := modelcontext.Resolve(capacity, modelcontext.Overrides{
		Window: config.ContextWindowTokens, Output: config.MaxOutputTokens, Trigger: config.CompactionTriggerTokens,
	})
	if err != nil {
		return nil, err
	}
	applyContextLimits(&config, limits)
	if config.Tokenizer == nil {
		config.Tokenizer = tokenizer.NewApproximate("")
	}
	goalMode := config.GoalMode.WithDefaults()
	if err := goalMode.Validate(); err != nil {
		return nil, err
	}
	if goalMode.Enabled && config.GoalStore == nil {
		return nil, errors.New("Goal Mode requires a durable goal store")
	}
	if goalMode.Enabled {
		if _, ok := config.GoalStore.(agentgoal.AdmissionRepository); !ok {
			return nil, errors.New("Goal Mode requires a durable admission repository")
		}
	}
	factory := &Factory{
		config:       config,
		model:        chatModel,
		registry:     registry,
		pipeline:     pipeline,
		events:       events,
		checkpoints:  checkpoints,
		tokenizer:    config.Tokenizer,
		calibration:  &contextCalibration{},
		capacity:     capacity,
		outputPolicy: outputPolicy,
		goalMode:     goalMode,
		goalStore:    config.GoalStore,
		goalBudget:   config.GoalBudget,
		artifacts:    config.ContextArtifacts,
	}
	if goalMode.Enabled {
		composed, err := factory.GoalEvaluator(goalMode.Assessment)
		if err != nil {
			return nil, err
		}
		factory.goalEval = composed
	}
	return factory, nil
}

func modelCapacity(chatModel model.BaseChatModel, provider, name string) modelcontext.Metadata {
	capacity := modelcontext.Lookup(provider, name)
	if informer, ok := chatModel.(modelcontext.Informer); ok {
		declared := informer.ModelContext()
		if declared.Window > 1 {
			capacity.Window, capacity.Source = declared.Window, declared.Source
			if capacity.Source == "" {
				capacity.Source = modelcontext.SourceProvider
			}
		}
		if declared.MaxOutputTokens > 0 {
			capacity.MaxOutputTokens, capacity.OutputSource = declared.MaxOutputTokens, declared.OutputSource
			if capacity.OutputSource == "" {
				capacity.OutputSource = modelcontext.SourceProvider
			}
		}
	}
	return capacity
}

func applyContextLimits(config *Config, limits modelcontext.Limits) {
	config.ContextWindowTokens, config.ContextWindowSource = limits.Window, limits.WindowSource
	config.MaxOutputTokens, config.CompactionTriggerTokens = limits.OutputReserve, limits.Trigger
}

package app

import (
	"errors"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/model"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/strategy"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

// NewAgentFactory binds a provider model to Eino while preserving the
// application's event, policy and immutable tool boundaries.
func (a *App) NewAgentFactory(chatModel model.BaseChatModel, config agentruntime.Config, approvers ...policy.Approver) (*agentruntime.Factory, error) {
	if a == nil {
		return nil, errors.New("application Agent runtime is unavailable")
	}
	a.stateMu.Lock()
	if a.closed {
		a.stateMu.Unlock()
		return nil, errors.New("application is closed")
	}
	pipeline := a.pipeline
	registry := a.registry
	events := a.events
	checkpoints := a.checkpoints
	database := a.database
	artifacts := a.artifacts
	goalConfig := a.config.Goal
	prices := a.config.Usage.Prices
	workspace := a.workspace
	a.stateMu.Unlock()
	if pipeline == nil || registry == nil || events == nil || checkpoints == nil {
		return nil, errors.New("application Agent runtime is unavailable")
	}
	if len(approvers) > 1 {
		return nil, errors.New("at most one tool approver may be configured")
	}
	if len(approvers) == 1 {
		pipeline = pipeline.WithApprover(approvers[0])
	}
	config.GoalMode = goalConfig
	if config.GoalMode.Enabled {
		if database == nil {
			return nil, errors.New("application Goal runtime is unavailable")
		}
		config.GoalStore = database.Orchestration()
		config.GoalBudget = database.Orchestration()
	}
	if config.ContextArtifacts == nil {
		config.ContextArtifacts = artifacts
	}
	// Child factories inherit the table and price their own provider/model.
	if config.Pricing == nil && len(prices) > 0 {
		config.Pricing = tokenusage.NewPriceTable(prices)
	}
	config.Strategies = a.strategies
	if workspace != nil && config.ActorScope.ParentAgentID == "" {
		// Root context describes the actual tool boundary, not the process's
		// current directory or an inferred repository root. Child profiles
		// supply their own instruction and may use isolated workspace views.
		config.Instruction = strings.TrimSpace(config.Instruction + "\n\n" + rootWorkspaceInstruction(workspace.Root()))
	}
	manifest, err := registry.Snapshot().Manifest()
	if err != nil {
		return nil, err
	}
	config.StrategyScope = strategy.Digest([]string{manifest.Host, a.nativeScope, config.StrategyScope})
	factory, err := agentruntime.NewFactory(config, chatModel, registry, pipeline, events, checkpoints)
	if err != nil {
		return nil, err
	}
	if config.ActorScope.AgentID != "" && config.ActorScope.ParentAgentID == "" {
		a.stateMu.Lock()
		a.shadowFactories[config.ActorScope.AgentID] = factory
		a.stateMu.Unlock()
	}
	return factory, nil
}

func rootWorkspaceInstruction(root string) string {
	return fmt.Sprintf(`Host workspace context:
The effective workspace root is %q. Workspace tool paths and shell working_directory are relative to this root; use "." for the root itself. An absolute path may be converted to a relative path only if it is inside this root.
If the requested target is outside this workspace, stop and explain the boundary. Ask the user to restart with --workspace pointing to the intended project, or provide the file contents. Do not try parent paths or shell commands to bypass the workspace boundary.`, root)
}

package app

import (
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentprofile"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
)

// Profiles returns the immutable-generation Agent profile registry.
func (a *App) Profiles() *agentprofile.Registry {
	if a == nil {
		return nil
	}
	return a.profiles
}

func newAgentProfiles(resolved config.Config) (*agentprofile.Registry, error) {
	registry := agentprofile.NewRegistry()
	modelProfile := resolved.Model.Name
	if modelProfile == "" {
		modelProfile = "default"
	}
	maxIterations := resolved.Agent.MaxIterations
	if maxIterations <= 0 {
		maxIterations = 20
	}
	limits := agentprofile.Limits{
		MaxIterations: maxIterations,
		MaxChildren:   4,
		MaxToolCalls:  maxIterations * 8,
		MaxWallTime:   30 * time.Minute,
	}
	if _, err := registry.Activate(
		agentprofile.Profile{
			Name: "default", Revision: "default-v1", ModelProfile: modelProfile,
			Instruction: resolved.Agent.Instruction, ContextInheritance: agentprofile.InheritNone,
			WorkspaceMode: agentprofile.WorkspaceSharedLease, Limits: limits,
			CanSpawn: true, CanManageTasks: true, CanSendMessages: true,
		},
		agentprofile.Profile{
			Name: "worker", Revision: "worker-v1", ModelProfile: modelProfile,
			Instruction: resolved.Agent.Instruction, ContextInheritance: agentprofile.InheritSummary,
			WorkspaceMode: agentprofile.WorkspaceSharedLease, Limits: limits,
			CanSpawn: true, CanManageTasks: true, CanSendMessages: true,
		},
		agentprofile.Profile{
			Name: "reviewer", Revision: "reviewer-v1", ModelProfile: modelProfile,
			Instruction: resolved.Agent.Instruction, ContextInheritance: agentprofile.InheritSummary,
			WorkspaceMode: agentprofile.WorkspaceSharedReadonly, Limits: limits,
			CanSpawn: false, CanManageTasks: true, CanSendMessages: true,
		},
	); err != nil {
		return nil, err
	}
	return registry, nil
}

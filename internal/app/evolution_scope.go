package app

import (
	"context"
	"errors"

	"github.com/vkviyu/go-self-evolving-agent/internal/evolutiontools"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/strategy"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

// strategyCaller is the host authorization boundary. Resolve a durable root
// and its configured factory, never identities or model settings from JSON.
func (a *App) strategyCaller(ctx context.Context) (evolutiontools.StrategyCaller, error) {
	invocation, ok := tool.InvocationFromContext(ctx)
	scope, scoped := orchestration.ActorScopeFromContext(ctx)
	if !ok || !scoped || invocation.SessionID == "" || scope.AgentID == "" || scope.ParentAgentID != "" {
		return evolutiontools.StrategyCaller{}, errors.New("strategy evolution requires a configured root Agent")
	}
	root, err := a.repositories.RootThreadBySession(ctx, invocation.SessionID)
	if err != nil || root.ActorScope() != scope {
		return evolutiontools.StrategyCaller{}, errors.New("strategy caller does not match the session's root Agent")
	}
	a.stateMu.Lock()
	factory := a.shadowFactories[scope.AgentID]
	a.stateMu.Unlock()
	if factory == nil {
		return evolutiontools.StrategyCaller{}, errors.New("root model factory is unavailable")
	}
	return evolutiontools.StrategyCaller{
		OwnerSession:     string(invocation.SessionID),
		BaselineConfigID: factory.BaselineConfigID(),
		Compare: func(ctx context.Context, candidateID, suiteID string) (strategy.Report, string, error) {
			return a.ShadowStrategy(ctx, factory, candidateID, suiteID)
		},
	}, nil
}

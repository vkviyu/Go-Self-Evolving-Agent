package app

import (
	"context"
	"errors"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/strategy"
)

// Strategies is the host management API, independent of its SQLite adapter.
// Agents only receive scoped tools and cannot register their own expectations.
func (a *App) Strategies() strategy.Catalog { return a.strategies }

// ShadowStrategy compares a candidate strategy with its baseline on a supplied fixture
// suite.
func (a *App) ShadowStrategy(ctx context.Context, factory *agentruntime.Factory, candidateID, suiteID string) (strategy.Report, string, error) {
	ctx, finish, err := a.beginEvolution(ctx)
	if err != nil {
		return strategy.Report{}, "", err
	}
	defer finish()
	// Check concrete nil before converting to the Comparator interface.
	if factory == nil {
		return strategy.Report{}, "", errors.New("shadow comparison requires a configured model factory")
	}
	return a.shadow.Compare(ctx, factory, candidateID, suiteID)
}

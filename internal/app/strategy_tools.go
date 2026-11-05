package app

import (
	"github.com/vkviyu/go-self-evolving-agent/internal/evolutiontools"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func strategyEvolutionBindings(a *App) []tool.Binding {
	return evolutiontools.StrategyBindings(a.Strategies(), a.strategyCaller)
}

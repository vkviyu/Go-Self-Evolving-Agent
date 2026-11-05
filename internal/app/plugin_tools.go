package app

import (
	"github.com/vkviyu/go-self-evolving-agent/internal/evolutiontools"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func pluginEvolutionBindings(application *App) []tool.Binding {
	return evolutiontools.PluginBindings(application)
}

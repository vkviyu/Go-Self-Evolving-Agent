package app

import (
	"context"

	"github.com/vkviyu/go-self-evolving-agent/internal/pluginbuild"
)

// BuildPlugin attaches the build service to the App's shutdown boundary.
// The service owns workflow ordering; the App owns cancellation and joining.
func (a *App) BuildPlugin(ctx context.Context, request pluginbuild.Request) (pluginbuild.Result, error) {
	ctx, finish, err := a.beginEvolution(ctx)
	if err != nil {
		return pluginbuild.Result{}, err
	}
	defer finish()
	return a.builds.Build(ctx, request)
}

func (a *App) PluginBuild(ctx context.Context, id string) (pluginbuild.Request, *pluginbuild.Result, error) {
	return a.builds.Inspect(ctx, id)
}

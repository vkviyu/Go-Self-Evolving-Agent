package app

import (
	"context"

	"github.com/vkviyu/go-self-evolving-agent/internal/plugin"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func (a *App) VerifyPlugin(ctx context.Context, name, revision string, cases []plugin.TestCase) (plugin.TestReport, string, error) {
	return a.publication.Verify(ctx, name, revision, cases)
}

// PromotePlugin is a host boundary. The Agent adapter may call it only after
// the existing Tool Pipeline has obtained approval for the exact candidate.
func (a *App) PromotePlugin(ctx context.Context, name, revision, reportID string, confirmed bool) (*tool.Snapshot, error) {
	return a.publication.Promote(ctx, name, revision, reportID, confirmed)
}

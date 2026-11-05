// Package evolutiontools adapts model-facing JSON tools to host services.
// It owns neither an Agent Loop nor a database, compiler or runtime lifecycle.
// All bindings must execute through tool.Pipeline for policy and approval.
package evolutiontools

import (
	"context"

	"github.com/vkviyu/go-self-evolving-agent/internal/pluginbuild"
	"github.com/vkviyu/go-self-evolving-agent/internal/strategy"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

// PluginHost retains worker cancellation/join and publication ownership.
type PluginHost interface {
	BuildPlugin(context.Context, pluginbuild.Request) (pluginbuild.Result, error)
	PluginBuild(context.Context, string) (pluginbuild.Request, *pluginbuild.Result, error)
	PromotePlugin(context.Context, string, string, string, bool) (*tool.Snapshot, error)
}

// StrategyStore deliberately excludes trusted-suite registration and fallback.
// Runtime access and broader host maintenance use their own domain ports.
type StrategyStore interface {
	Save(context.Context, strategy.Spec) (string, error)
	Load(context.Context, string) (strategy.Spec, error)
	Report(context.Context, string) (strategy.Report, error)
	Promote(context.Context, string, bool) error
}

// StrategyCaller is constructed by the host from the trusted invocation and
// durable root identity. Compare is bound to that root's configured factory.
// It is not an input DTO and must never be decoded from model-supplied JSON.
type StrategyCaller struct {
	OwnerSession     string
	BaselineConfigID string
	Compare          func(context.Context, string, string) (strategy.Report, string, error)
}

type ResolveStrategyCaller func(context.Context) (StrategyCaller, error)

type pluginAdapter struct{ host PluginHost }

type strategyAdapter struct {
	store  StrategyStore
	caller ResolveStrategyCaller
}

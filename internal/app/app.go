// Package app is the composition root for the trusted local Agent runtime.
package app

import (
	"context"
	"sync"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentflow"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentmanager"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentprofile"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/approval"
	"github.com/vkviyu/go-self-evolving-agent/internal/artifact"
	"github.com/vkviyu/go-self-evolving-agent/internal/codingtools"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/lsp"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/plugin"
	"github.com/vkviyu/go-self-evolving-agent/internal/plugin/wasm"
	"github.com/vkviyu/go-self-evolving-agent/internal/pluginbuild"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimeowner"
	"github.com/vkviyu/go-self-evolving-agent/internal/sandbox"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/sqlitestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/strategy"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacedomain"
)

// App owns component lifetimes. Construction either succeeds completely or
// releases every resource acquired on the failed path.
type App struct {
	config           config.Config
	database         *sqlitestore.Database
	events           session.EventStore
	checkpoints      session.CheckpointStore
	registry         *tool.Registry
	pipeline         *tool.Pipeline
	wasm             *wasm.Runtime
	plugins          *plugin.Manager
	approval         *approval.Broker
	supervisor       *agentmanager.Supervisor
	profiles         *agentprofile.Registry
	workspace        *codingtools.Workspace
	coordinator      *workspacecoord.Coordinator
	artifacts        *artifact.Store
	diagnostics      *lsp.Manager
	flows            *agentflow.Runtime
	flowCatalog      *agentflow.Catalog
	builds           *pluginbuild.Service
	publication      *plugin.PublicationService
	strategies       strategy.Catalog
	shadow           *strategy.ShadowService
	shadowFactories  map[orchestration.AgentID]*agentruntime.Factory
	evolutionContext context.Context
	evolutionCancel  context.CancelFunc
	evolutionWork    sync.WaitGroup

	stateMu     sync.Mutex
	closed      bool
	closeOnce   sync.Once
	closeErr    error
	owner       *runtimeowner.Lock
	domain      *workspacedomain.Domain
	nativeScope string
}

// OpenOptions contains invocation-scoped trust-boundary choices that must not
// be persisted in user or project configuration.
type OpenOptions struct {
	ShellSandbox sandbox.Mode
	ToolBindings []tool.Binding
}

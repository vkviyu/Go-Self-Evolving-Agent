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
	"github.com/vkviyu/go-self-evolving-agent/internal/atebridge"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateprocess"
	"github.com/vkviyu/go-self-evolving-agent/internal/codingtools"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/experience"
	"github.com/vkviyu/go-self-evolving-agent/internal/lsp"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimeowner"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/sandbox"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/strategy"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacedomain"
)

// App owns component lifetimes. Construction either succeeds completely or
// releases every resource acquired on the failed path.
type App struct {
	// database owns local runtime resources and unresolved native requests.
	// Business repositories and session history are authoritative in ATE.
	database         *runtimestore.Database
	authority        *authorityResources
	sessionOwners    map[session.ID]*runtimeowner.Lock
	ownedTeams       map[orchestration.TeamID]struct{}
	config           config.Config
	repositories     OrchestrationRepository
	events           session.EventStore
	runtime          runtimeStorage
	registry         *tool.Registry
	pipeline         *tool.Pipeline
	approval         *approval.Broker
	supervisor       *agentmanager.Supervisor
	profiles         *agentprofile.Registry
	workspace        *codingtools.Workspace
	coordinator      *workspacecoord.Coordinator
	artifacts        *artifact.Store
	diagnostics      *lsp.Manager
	temporalProcess  *ateprocess.Manager
	temporalBridge   *atebridge.Bridge
	experience       *experience.Service
	flows            *agentflow.Runtime
	flowCatalog      *agentflow.Catalog
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

// runtimeStorage holds host execution resources. Session events, control-plane
// records and their recovery facts live in the authoritative ATE repositories.
type runtimeStorage struct {
	checkpoints session.CheckpointStore
	snapshots   runtimeSnapshotStore
	strategies  strategy.Catalog
	diagnostics lsp.ProjectionStore
}

type runtimeSnapshotStore interface {
	tool.SnapshotStore
	References(context.Context, string) ([]tool.SnapshotReference, error)
	ReferenceHistory(context.Context, string) ([]tool.SnapshotReferenceRecord, error)
}

// OpenOptions contains invocation-scoped trust-boundary choices that must not
// be persisted in user or project configuration.
type OpenOptions struct {
	ShellSandbox sandbox.Mode
	ToolBindings []tool.Binding
}

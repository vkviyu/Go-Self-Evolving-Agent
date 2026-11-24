package app

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentflow"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentprofile"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/approval"
	"github.com/vkviyu/go-self-evolving-agent/internal/artifact"
	"github.com/vkviyu/go-self-evolving-agent/internal/codingtools"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/evolutionstore"
	"github.com/vkviyu/go-self-evolving-agent/internal/lsp"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	orchestrationate "github.com/vkviyu/go-self-evolving-agent/internal/orchestration/atestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimeowner"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/strategy"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacedomain"
)

// Open builds the trusted runtime in dependency order.
func Open(ctx context.Context, unresolved config.Config) (*App, error) {
	return OpenWithOptions(ctx, unresolved, OpenOptions{})
}

// OpenWithOptions builds the trusted runtime with explicit invocation-scoped
// options. The default remains the host operating-system sandbox.
func OpenWithOptions(ctx context.Context, unresolved config.Config, options OpenOptions) (*App, error) {
	resolved, err := unresolved.Resolve()
	if err != nil {
		return nil, fmt.Errorf("resolve application config: %w", err)
	}
	owner, err := runtimeowner.Acquire(resolved.RuntimeDatabasePath())
	if err != nil {
		return nil, err
	}
	database, err := runtimestore.OpenRuntime(ctx, resolved.RuntimeDatabasePath())
	if err != nil {
		_ = owner.Close()
		return nil, err
	}
	authority, err := openAuthority(ctx, resolved, database.ATERequests())
	if err != nil {
		_ = database.Close()
		_ = owner.Close()
		return nil, err
	}
	repositories, err := orchestrationate.New(authority.records, authority.sessions)
	if err != nil {
		_ = authority.process.Close()
		_ = database.Close()
		_ = owner.Close()
		return nil, err
	}
	evolution, err := evolutionstore.New(authority.records)
	if err != nil {
		_ = authority.process.Close()
		_ = database.Close()
		_ = owner.Close()
		return nil, err
	}
	runtime := runtimeStorage{
		checkpoints: database.Checkpoints(),
		snapshots:   database.RuntimeSnapshots().WithReferenceGuard(repositories.CheckSnapshotReference),
		strategies:  evolution,
		diagnostics: database.LSPDiagnostics(),
	}
	closeOnError := true
	var diagnosticManager *lsp.Manager
	var domain *workspacedomain.Domain
	temporalCleanup := func() { _ = authority.process.Close() }
	evolutionContext, evolutionCancel := context.WithCancel(context.Background())
	approvalBroker := approval.NewBroker(16, approval.WithSourceDescriber(approvalSource(repositories)))
	defer func() {
		if closeOnError {
			evolutionCancel()
			if temporalCleanup != nil {
				temporalCleanup()
			}
			closeOpenResources(database, approvalBroker, diagnosticManager)
			_ = domain.Close()
			_ = owner.Close()
		}
	}()
	events := authority.sessions
	workspace, err := codingtools.NewWithSandboxMode(resolved.WorkspaceDirectory, options.ShellSandbox)
	if err != nil {
		return nil, fmt.Errorf("initialize workspace tools: %w", err)
	}
	// Join the workspace coordination domain before any tool can write, so
	// leases are visible to every instance on this workspace, not only to
	// this Session's database.
	domain, err = workspacedomain.Open(ctx, workspacedomain.Options{
		HomeDirectory:       resolved.CoordinationHomeDirectory(),
		RendezvousDirectory: resolved.CoordinationDirectory,
		WorkspaceRoot:       workspace.Root(),
		DataDirectory:       resolved.DataDirectory,
		ValidateOwner:       leaseOwnerValidator(repositories),
	})
	if err != nil {
		return nil, fmt.Errorf("join workspace coordination domain: %w", err)
	}
	workspace.AttachLeaseRepository(domain.Leases())
	workspace.AttachDescendantLiveness(domain)
	workspace.AttachWorkspaceViewRepository(repositories)
	if resolved.LSP.Enabled {
		diagnosticManager, err = lsp.NewManager(resolved.LSP)
		if err != nil {
			return nil, fmt.Errorf("initialize language-server diagnostics: %w", err)
		}
		diagnosticManager.AttachProjectionStore(runtime.diagnostics)
		workspace.AttachLanguageDiagnostics(diagnosticManager)
	}
	worktrees, err := workspacecoord.NewWorktreeManager(workspace.Root())
	if err != nil {
		return nil, fmt.Errorf("initialize workspace worktree manager: %w", err)
	}
	coordinator, err := workspacecoord.NewCoordinator(repositories, worktrees)
	if err != nil {
		return nil, fmt.Errorf("initialize workspace coordinator: %w", err)
	}
	// Artifacts belong to the Session runtime, not the user workspace. This
	// keeps multiple Sessions over the same source tree from sharing or
	// accidentally committing runtime-owned files.
	artifacts, err := artifact.NewStore(filepath.Join(resolved.DataDirectory, "artifacts"))
	if err != nil {
		return nil, fmt.Errorf("initialize workspace artifact store: %w", err)
	}
	workspace.AttachDiffArtifactStore(artifacts)
	coordinator.Artifacts = artifacts
	coordinator.Events = repositories
	// Snapshot views live in the private data directory, outside the
	// workspace, so their work trees never appear in the main tree.
	views := filepath.Join(resolved.DataDirectory, "views")
	if err := workspace.AttachSnapshotViewsDirectory(views); err != nil {
		return nil, fmt.Errorf("initialize snapshot views: %w", err)
	}
	coordinator.Snapshots = &workspacecoord.SnapshotStore{
		ViewsDirectory: views, WorkspaceRoot: workspace.Root(),
		CopyLimit: resolved.Agents.SnapshotCopyLimit, Barrier: snapshotBarrier(domain.Leases()),
	}
	policyEngine, err := buildPolicy(options.ToolBindings)
	if err != nil {
		return nil, err
	}
	registry := tool.NewRegistry()
	nativeScope, err := nativeSnapshotScope(resolved, workspace.Root(), options.ShellSandbox)
	if err != nil {
		return nil, fmt.Errorf("identify native tool environment: %w", err)
	}
	profiles, err := newAgentProfiles(resolved)
	if err != nil {
		return nil, fmt.Errorf("activate built-in Agent profiles: %w", err)
	}
	// Bind closures to an allocated owner with stable dependencies. Complete
	// remaining services before returning; never overwrite its mutex state.
	application := &App{
		evolutionContext: evolutionContext,
		evolutionCancel:  evolutionCancel,
		shadowFactories:  make(map[orchestration.AgentID]*agentruntime.Factory),
		owner:            owner,
		domain:           domain,
		nativeScope:      nativeScope,
		config:           resolved,
		database:         database,
		authority:        authority,
		temporalProcess:  authority.process,
		repositories:     repositories,
		events:           events,
		runtime:          runtime,
		registry:         registry,
		approval:         approvalBroker,
		profiles:         profiles,
		workspace:        workspace,
		coordinator:      coordinator,
		artifacts:        artifacts,
		diagnostics:      diagnosticManager,
		strategies:       runtime.strategies,
	}
	if err := application.openTemporal(ctx); err != nil {
		return nil, err
	}
	if application.temporalProcess != nil {
		temporalCleanup = func() { _ = application.temporalProcess.Close() }
	}
	bindings := buildBuiltinBindings(resolved, workspace, artifacts)
	bindings = append(bindings, memoryToolBindings(application.experience)...)
	bindings = append(bindings, strategyEvolutionBindings(application)...)
	bindings = append(bindings, options.ToolBindings...)
	bindings = append(bindings, agentflow.ControlBindings(application.repositories)...)
	if _, err := registry.ActivateMany(scopeNativeBindings(nativeScope, bindings)...); err != nil {
		return nil, fmt.Errorf("activate built-in tools: %w", err)
	}
	pipeline, err := tool.NewPipeline(tool.PipelineConfig{
		Registry:          registry,
		Policy:            policyEngine,
		Approver:          approvalBroker,
		Events:            events,
		DurabilityTimeout: resolved.ATE.Timeout + time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("construct tool pipeline: %w", err)
	}
	flowRuntime, err := agentflow.NewRuntime(agentflow.RuntimeConfig{Pipeline: pipeline, Registry: registry, Store: application.repositories})
	if err != nil {
		return nil, fmt.Errorf("construct Flow runtime: %w", err)
	}
	flowCatalog, err := agentflow.NewCatalog(flowRuntime)
	if err != nil {
		return nil, fmt.Errorf("construct Flow catalog: %w", err)
	}
	if _, err := registry.ActivateMany(scopeNativeBindings(nativeScope, agentflow.CatalogBindings(flowCatalog))...); err != nil {
		return nil, fmt.Errorf("activate Flow catalog tools: %w", err)
	}
	if err := registry.ConfigurePersistence(application.runtime.snapshots); err != nil {
		return nil, err
	}
	application.pipeline = pipeline
	application.flows, application.flowCatalog = flowRuntime, flowCatalog
	application.shadow, err = strategy.NewShadowService(application.strategies)
	if err != nil {
		return nil, err
	}
	closeOnError = false
	return application, nil
}

// leaseOwnerValidator admits lease requests only from Agents registered in
// this Session's Team. Agents come from the configured repositories; leases live
// in the shared workspace coordination database.
func leaseOwnerValidator(agents agentLookup) workspacedomain.OwnerValidator {
	return func(ctx context.Context, teamID orchestration.TeamID, agentID orchestration.AgentID) error {
		agent, err := agents.GetAgent(ctx, agentID)
		if err != nil {
			return err
		}
		if agent.TeamID != teamID {
			return fmt.Errorf("%w: lease Agent belongs to another Team", orchestration.ErrInvalidEvent)
		}
		return nil
	}
}

// approvalSource names the delegated Agent behind an approval request by its
// profile and task, so prompts stay attributable while children run in
// parallel. Lookup failures fall back to the identifiers in the actor scope.
func approvalSource(store approvalLookup) approval.SourceDescriber {
	return func(ctx context.Context, actor orchestration.ActorScope) approval.Source {
		if actor.ParentAgentID == "" || actor.AgentID == "" {
			return approval.Source{}
		}
		source := approval.Source{AgentID: actor.AgentID, Profile: actor.ProfileRevision}
		if agent, err := store.GetAgent(ctx, actor.AgentID); err == nil {
			if profile, err := agentprofile.Decode(agent.ProfileSnapshot, agent.ProfileRevision); err == nil {
				source.Profile = profile.Name
			}
		}
		if actor.TaskID != "" {
			if task, err := store.GetTask(ctx, actor.TaskID); err == nil {
				source.Task = task.Title
			}
		}
		return source
	}
}

package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentflow"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentmanager"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentprofile"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

// OrchestrationRepository composes the control-plane ports required by App's
// consumers. Services still accept their own narrower domain interfaces.
// Recovery extensions are explicit here: replacing an adapter must not silently
// disable capabilities discovered through type assertions by those services.
//
// ATE backs these ports. Runtime storage and execution ownership are separate.
type OrchestrationRepository interface {
	orchestration.RootThreadRepository
	orchestration.TeamRepository
	orchestration.AgentRepository
	orchestration.SpawnRequestFinder
	orchestration.AgentCompletionRepository
	orchestration.TaskRepository
	orchestration.TaskRefinementRepository
	orchestration.TaskReassignmentRepository
	orchestration.TaskPlanRepository
	orchestration.TaskClaimReaper
	orchestration.MailboxRepository
	orchestration.MailboxLedger
	orchestration.MessageRequestFinder
	orchestration.WaitRepository
	orchestration.EventStore
	agentflow.Store
	workspacecoord.Repository
	workspacecoord.ResolutionLinker
	workspacecoord.ApplyJournal
	workspacecoord.FinalizationStore
	tokenusage.DurableReservationLedger
	tokenusage.Reader
	teamRecoveryRepository

	Budget(context.Context, orchestration.TeamID) (tokenusage.BudgetSnapshot, error)

	LoadTeam(context.Context, orchestration.TeamID, orchestration.Cursor, int) ([]orchestration.Event, error)
	ConfigureBudget(context.Context, orchestration.TeamID, tokenusage.BudgetLimits) error
	ResumeAbandonments(context.Context, session.ID) error
	ResumeRetentions(context.Context, session.ID) error
}

// approvalLookup exposes only the reads needed to attribute approval requests.
type approvalLookup interface {
	agentLookup
	GetTask(context.Context, orchestration.TaskID) (orchestration.TaskSnapshot, error)
}

// Orchestration exposes the configured durable Multi-Agent repositories.
func (a *App) Orchestration() OrchestrationRepository {
	if a == nil {
		return nil
	}
	return a.repositories
}

// NewSupervisor installs the durable Agent control tools and returns the
// process-level lifecycle owner. It is called after the model runner exists
// because child execution is provider-specific.
func (a *App) NewSupervisor(config agentmanager.Config) (*agentmanager.Supervisor, error) {
	return a.NewSupervisorContext(config.RunContext, config)
}

// NewSupervisorContext uses recoveryContext for synchronous storage recovery.
// Config.RunContext independently owns the lifetime of background workers.
// For storage batches, set DeferRecoveryDispatch and release startup only after
// the batch exits.
func (a *App) NewSupervisorContext(recoveryContext context.Context, config agentmanager.Config) (*agentmanager.Supervisor, error) {
	if a == nil || a.repositories == nil || a.registry == nil {
		return nil, errors.New("application orchestration is unavailable")
	}
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if a.closed {
		return nil, errors.New("application is closed")
	}
	if a.supervisor != nil {
		return nil, errors.New("application Supervisor is already initialized")
	}
	repositories, err := a.supervisorRepositories()
	if err != nil {
		return nil, err
	}
	if config.Agents == nil {
		config.Agents = repositories
	}
	if config.Tasks == nil {
		config.Tasks = repositories
	}
	if config.Profiles == nil {
		config.Profiles = a.profiles
	}
	if config.Mailbox == nil {
		config.Mailbox = repositories
	}
	if config.WaitSets == nil {
		config.WaitSets = repositories
	}
	if config.UsageLedger == nil {
		config.UsageLedger = repositories
	}
	if config.Leases == nil && a.domain != nil {
		config.Leases = a.domain.Leases()
	}
	if config.Workspace == nil {
		config.Workspace = a.coordinator
	}
	if config.WorkspaceRoot == "" {
		config.WorkspaceRoot = a.config.WorkspaceDirectory
	}
	if config.DefaultWorkspace == "" {
		config.DefaultWorkspace = agentmanager.WorkspaceDefault(a.config.Agents.DefaultWorkspace)
	}
	if config.MergeExecutor == nil && a.coordinator != nil && (a.coordinator.Worktrees != nil || a.coordinator.Snapshots != nil) {
		// Without an approver, snapshot merges wait for auto merge or for a
		// session that supplies its own executor.
		mergeExecutor, mergeErr := a.newMergeExecutor(a.artifacts, nil)
		if mergeErr != nil {
			return nil, fmt.Errorf("initialize workspace merge executor: %w", mergeErr)
		}
		config.MergeExecutor = mergeExecutor
	}
	supervisor, err := agentmanager.NewSupervisor(config)
	if err != nil {
		return nil, err
	}
	bindings := scopeNativeBindings(a.nativeScope, agentmanager.Bindings(supervisor))
	if _, err := a.registry.ActivateManyWithCommit(func(previous, _ *tool.Snapshot) error {
		for _, binding := range bindings {
			if _, exists := previous.Get(binding.Descriptor.Name); exists {
				return fmt.Errorf("Agent control tool name is already owned: %s", binding.Descriptor.Name)
			}
		}
		return nil
	}, bindings...); err != nil {
		return nil, a.closeFailedSupervisor(recoveryContext, supervisor, fmt.Errorf("activate Agent control tools: %w", err))
	}
	if recoveryContext == nil {
		recoveryContext = context.Background()
	}
	if err := supervisor.Recover(recoveryContext); err != nil {
		err = a.closeFailedSupervisor(recoveryContext, supervisor, fmt.Errorf("recover durable child Agents: %w", err))
		retireBindings(a.registry, bindings)
		return nil, err
	}
	a.supervisor = supervisor
	return supervisor, nil
}

// Called with stateMu held. Failed assembly normally permits retry. If its
// workers cannot stop, retain their owner and quarantine this App instead of
// losing the Supervisor and later closing resources underneath it.
func (a *App) closeFailedSupervisor(ctx context.Context, supervisor *agentmanager.Supervisor, cause error) error {
	closeErr := supervisor.CloseContext(ctx)
	if errors.Is(closeErr, agentmanager.ErrSupervisorCloseTimeout) {
		a.supervisor = supervisor
		a.closed = true
		if a.evolutionCancel != nil {
			a.evolutionCancel()
		}
		a.retainUntilExit()
	}
	return errors.Join(cause, closeErr)
}

// retireBindings removes only the revisions published for a failed
// Supervisor construction. Checking the active revision first avoids
// clobbering a concurrent replacement published after activation.
func retireBindings(registry *tool.Registry, bindings []tool.Binding) {
	if registry == nil {
		return
	}
	_, _ = registry.RetireMany(bindings...)
}

// EnsureRootThreadOptions controls optional startup work around root creation.
// Interactive chat may defer maintenance until its terminal surface is ready;
// the first turn still waits for that maintenance to finish.
type EnsureRootThreadOptions struct {
	DeferMaintenance bool
}

// EnsureRootThread creates the authoritative single-Agent root used by the
// chat command and returns its stable actor scope. It also applies
// [usage.budget] to the root Team; the budget counts all usage of the Team,
// including earlier runs of a resumed session.
func (a *App) EnsureRootThread(ctx context.Context, sessionID session.ID) (orchestration.RootThread, bool, error) {
	return a.EnsureRootThreadWithOptions(ctx, sessionID, EnsureRootThreadOptions{})
}

// EnsureRootThreadWithOptions is the configurable form of EnsureRootThread.
func (a *App) EnsureRootThreadWithOptions(ctx context.Context, sessionID session.ID, options EnsureRootThreadOptions) (orchestration.RootThread, bool, error) {
	if a == nil || a.repositories == nil {
		return orchestration.RootThread{}, false, errors.New("application authority is unavailable")
	}
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if a.closed {
		return orchestration.RootThread{}, false, errors.New("application is closed")
	}
	if err := a.ownSession(sessionID); err != nil {
		return orchestration.RootThread{}, false, err
	}
	// Hold execution ownership until root creation and budget configuration
	// finish. Close must not release the Session lock underneath either write.
	repositories := a.repositories
	budget := a.config.Usage.Budget
	if ctx == nil {
		ctx = context.Background()
	}
	if err := a.recoverRootSessionRequests(ctx, sessionID); err != nil {
		return orchestration.RootThread{}, false, err
	}
	if !options.DeferMaintenance {
		if err := a.resumeOwnedSessionMaintenance(ctx, sessionID); err != nil {
			return orchestration.RootThread{}, false, err
		}
	}
	profile, ok := a.profiles.Snapshot().Get("default")
	if !ok {
		return orchestration.RootThread{}, false, errors.New("default Agent profile is unavailable")
	}
	snapshot, err := agentprofile.Encode(profile)
	if err != nil {
		return orchestration.RootThread{}, false, err
	}
	root, created, err := repositories.EnsureRootThread(ctx, orchestration.RootThreadSeed{
		SessionID:       sessionID,
		ProfileRevision: profile.Revision, ProfileSnapshot: snapshot,
	})
	if err != nil {
		return orchestration.RootThread{}, false, err
	}
	// [usage.budget] is the only source of Team budgets, so every start
	// rewrites it; zero limits clear a budget removed from the config.
	if err := repositories.ConfigureBudget(ctx, root.TeamID, budget); err != nil {
		return orchestration.RootThread{}, false, err
	}
	if a.ownedTeams == nil {
		a.ownedTeams = make(map[orchestration.TeamID]struct{})
	}
	a.ownedTeams[root.TeamID] = struct{}{}
	return root, created, nil
}

// ResumeSessionMaintenance completes deferred recovery for an owned Session.
// It is a lifecycle operation owned by App so callers cannot run maintenance
// after the authority or Session ownership has been released.
func (a *App) ResumeSessionMaintenance(ctx context.Context, sessionID session.ID) error {
	if a == nil {
		return errors.New("application is unavailable")
	}
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if a.closed {
		return errors.New("application is closed")
	}
	return a.resumeOwnedSessionMaintenance(ctx, sessionID)
}

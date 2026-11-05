package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentmanager"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentprofile"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/sqlitestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

// Orchestration exposes the durable Multi-Agent control-plane store.
func (a *App) Orchestration() *sqlitestore.OrchestrationStore {
	if a == nil || a.database == nil {
		return nil
	}
	return a.database.Orchestration()
}

// NewSupervisor installs the durable Agent control tools and returns the
// process-level lifecycle owner. It is called after the model runner exists
// because child execution is provider-specific.
func (a *App) NewSupervisor(config agentmanager.Config) (*agentmanager.Supervisor, error) {
	if a == nil || a.database == nil || a.registry == nil {
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
	if config.Agents == nil {
		config.Agents = a.database.Orchestration()
	}
	if config.Tasks == nil {
		config.Tasks = a.database.Orchestration()
	}
	if config.Profiles == nil {
		config.Profiles = a.profiles
	}
	if config.Mailbox == nil {
		config.Mailbox = a.database.Orchestration()
	}
	if config.WaitSets == nil {
		config.WaitSets = a.database.Orchestration()
	}
	if config.UsageLedger == nil {
		config.UsageLedger = a.database.Orchestration()
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
		return nil, a.closeFailedSupervisor(supervisor, fmt.Errorf("activate Agent control tools: %w", err))
	}
	if err := supervisor.Recover(context.Background()); err != nil {
		err = a.closeFailedSupervisor(supervisor, fmt.Errorf("recover durable child Agents: %w", err))
		retireBindings(a.registry, bindings)
		return nil, err
	}
	a.supervisor = supervisor
	return supervisor, nil
}

// Called with stateMu held. Failed assembly normally permits retry. If its
// workers cannot stop, retain their owner and quarantine this App instead of
// losing the Supervisor and later closing resources underneath it.
func (a *App) closeFailedSupervisor(supervisor *agentmanager.Supervisor, cause error) error {
	closeErr := supervisor.Close()
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

// EnsureRootThread creates the durable single-Agent projection used by the
// existing chat command and returns its stable actor scope. It also applies
// [usage.budget] to the root Team; the budget counts all usage of the Team,
// including earlier runs of a resumed session.
func (a *App) EnsureRootThread(ctx context.Context, sessionID session.ID) (orchestration.RootThread, bool, error) {
	if a == nil || a.database == nil {
		return orchestration.RootThread{}, false, errors.New("application database is unavailable")
	}
	a.stateMu.Lock()
	if a.closed {
		a.stateMu.Unlock()
		return orchestration.RootThread{}, false, errors.New("application is closed")
	}
	database := a.database
	budget := a.config.Usage.Budget
	a.stateMu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	profile, ok := a.profiles.Snapshot().Get("default")
	if !ok {
		return orchestration.RootThread{}, false, errors.New("default Agent profile is unavailable")
	}
	snapshot, err := agentprofile.Encode(profile)
	if err != nil {
		return orchestration.RootThread{}, false, err
	}
	root, created, err := database.Orchestration().EnsureRootThread(ctx, orchestration.RootThreadSeed{
		SessionID:       sessionID,
		ProfileRevision: profile.Revision, ProfileSnapshot: snapshot,
	})
	if err != nil {
		return orchestration.RootThread{}, false, err
	}
	// [usage.budget] is the only source of Team budgets, so every start
	// rewrites it; zero limits clear a budget removed from the config.
	if err := database.Orchestration().ConfigureBudget(ctx, root.TeamID, budget); err != nil {
		return orchestration.RootThread{}, false, err
	}
	return root, created, nil
}

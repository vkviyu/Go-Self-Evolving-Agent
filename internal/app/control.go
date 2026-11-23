package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentmanager"
	"github.com/vkviyu/go-self-evolving-agent/internal/artifact"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateprocess"
	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
	"github.com/vkviyu/go-self-evolving-agent/internal/codingtools"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	orchestrationate "github.com/vkviyu/go-self-evolving-agent/internal/orchestration/atestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimeowner"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimestore"
	sessionate "github.com/vkviyu/go-self-evolving-agent/internal/session/atestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacedomain"
)

// OpenControl opens existing ATE records without a runtime database, Session
// ownership, native request replay, or executable services. One invocation owns
// one command; Close releases its authority connection and any later ownership.
func OpenControl(ctx context.Context, cfg config.Config) (_ *App, resultErr error) {
	resolved, err := cfg.Resolve()
	if err != nil {
		return nil, err
	}
	if !resolved.ATE.Enabled {
		return nil, errors.New("ATE is required for control commands; enable ate.enabled")
	}
	if _, err := os.Stat(resolved.ATE.Directory); err != nil {
		return nil, fmt.Errorf("open existing control authority: %w", err)
	}
	process, err := ateprocess.New(ateprocess.Options{
		Binary: resolved.ATE.Binary, Directory: resolved.ATE.Directory, Timeout: resolved.ATE.Timeout,
	})
	if err != nil {
		return nil, err
	}
	a := &App{config: resolved, temporalProcess: process}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, a.Close())
		}
	}()
	lease, err := process.OpenClient(ctx)
	if err != nil {
		return nil, err
	}
	resources := &authorityResources{process: process, lease: lease}
	bound := authorityProcess{manager: process, lease: lease}
	err = lease.WithClientContext(ctx, func(operation context.Context, c *client.Client) error {
		if c.Identity().Principal != "gsea" {
			return errors.New("unexpected ATE authenticated principal")
		}
		snapshot, err := c.Snapshot(operation)
		if err != nil {
			return err
		}
		space, err := c.GetSpace(operation, api.GetSpaceRequest{Space: authoritySpace, Principal: "gsea"})
		if err != nil {
			return err
		}
		if !space.Found {
			return errors.New("control authority has not been initialized")
		}
		resources.storeID = snapshot.Fence.Store
		return nil
	})
	if err != nil {
		return nil, err
	}
	bound.storeID = resources.storeID
	resources.bound = bound
	a.authority = resources
	if err := a.controlRepositories(controlReadOnlyPending{}); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *App) controlRepositories(pending aterequest.PendingStore) error {
	records, err := ateauthority.New(ateauthority.Options{
		Process: a.authority.bound, Pending: pending, Space: authoritySpace, Principal: "gsea",
	})
	if err != nil {
		return err
	}
	sessions, err := sessionate.New(sessionate.Options{
		Process: a.authority.bound, Pending: pending, Space: authoritySpace, StoreID: a.authority.storeID,
	})
	if err != nil {
		return err
	}
	repositories, err := orchestrationate.New(records, sessions)
	if err != nil {
		return err
	}
	a.authority.records, a.authority.sessions = records, sessions
	a.repositories, a.events = repositories, sessions
	return nil
}

// AttachExistingTeam acquires exactly the same root Session lease as chat.
// Unlike EnsureRootThread it never creates/reopens a Team or configures budgets.
func (a *App) AttachExistingTeam(ctx context.Context, teamID orchestration.TeamID) error {
	if a == nil || a.repositories == nil {
		return errors.New("application authority is unavailable")
	}
	if err := teamID.Validate(); err != nil {
		return err
	}
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if a.closed || a.supervisor != nil {
		return errors.New("cannot attach a Team to a closed or started application")
	}
	root, err := a.existingControlRoot(ctx, teamID)
	if err != nil {
		return err
	}
	if err := a.ownSession(root.SessionID); err != nil {
		return err
	}
	current, err := a.existingControlRoot(ctx, teamID)
	if err != nil {
		return err
	}
	if current.SessionID != root.SessionID || current.AgentID != root.AgentID || current.ThreadID != root.ThreadID {
		return errors.New("Team root changed while acquiring execution ownership")
	}
	if a.ownedTeams == nil {
		a.ownedTeams = make(map[orchestration.TeamID]struct{})
	}
	a.ownedTeams[teamID] = struct{}{}
	return nil
}

func (a *App) existingControlRoot(ctx context.Context, teamID orchestration.TeamID) (orchestration.RootThread, error) {
	team, err := a.repositories.GetTeam(ctx, teamID)
	if err != nil {
		return orchestration.RootThread{}, err
	}
	agent, err := a.repositories.GetAgent(ctx, team.RootAgentID)
	if err != nil {
		return orchestration.RootThread{}, err
	}
	if team.ID != teamID || agent.ID != team.RootAgentID || agent.TeamID != teamID || agent.ParentAgentID != "" || agent.SessionID == "" {
		return orchestration.RootThread{}, errors.New("invalid authoritative Team root")
	}
	root, err := a.repositories.RootThreadBySession(ctx, agent.SessionID)
	if err != nil {
		return root, err
	}
	if root.TeamID != teamID || root.AgentID != agent.ID || root.SessionID != agent.SessionID || root.ThreadID != agent.ThreadID {
		return root, errors.New("Team root Session binding mismatch")
	}
	return root, nil
}

// NewControlSupervisor promotes a control reader only after target validation
// and attachment. No model runner is installed. Only saved Session plans whose
// ownership follows from the target Team may be recovered.
func (a *App) NewControlSupervisor(ctx context.Context, teamID orchestration.TeamID) (*agentmanager.Supervisor, error) {
	if err := a.prepareControlSupervisor(ctx, teamID); err != nil {
		return nil, err
	}
	return a.NewSupervisor(agentmanager.Config{
		RunContext: ctx, Leases: controlLeases{LeaseRepository: a.domain.Leases()},
	})
}

func (a *App) prepareControlSupervisor(ctx context.Context, teamID orchestration.TeamID) error {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if a.closed || a.supervisor != nil || a.database != nil {
		return errors.New("control application is already initialized or closed")
	}
	if _, ok := a.ownedTeams[teamID]; !ok || len(a.ownedTeams) != 1 {
		return errors.New("control requires ownership of exactly its target Team")
	}
	var err error
	a.owner, err = runtimeowner.Acquire(a.config.RuntimeDatabasePath())
	if err != nil {
		return err
	}
	a.database, err = runtimestore.OpenRuntime(ctx, a.config.RuntimeDatabasePath())
	if err != nil {
		return err
	}
	pending := a.database.ATERequests()
	if err := pending.BindAuthority(ctx, a.authority.storeID); err != nil {
		return err
	}
	if err := a.controlRepositories(pending); err != nil {
		return err
	}
	root, err := a.existingControlRoot(ctx, teamID)
	if err != nil {
		return err
	}
	if err := a.recoverRootSessionRequests(ctx, root.SessionID); err != nil {
		return err
	}
	records, err := pending.List(ctx, aterequest.ListOptions{Limit: 1, MaxBytes: aterequest.MaxListBytes})
	if err != nil {
		return err
	}
	if len(records) != 0 {
		return errors.New("pending ATE commands require execution-owner scope reconciliation before control mutations")
	}
	if err := a.resumeControlMaintenance(ctx, teamID); err != nil {
		return err
	}
	a.registry = tool.NewRegistry()
	a.profiles, err = newAgentProfiles(a.config)
	if err != nil {
		return err
	}
	a.workspace, err = codingtools.New(a.config.WorkspaceDirectory)
	if err != nil {
		return err
	}
	a.domain, err = workspacedomain.Open(ctx, workspacedomain.Options{
		HomeDirectory: a.config.CoordinationHomeDirectory(), RendezvousDirectory: a.config.CoordinationDirectory,
		WorkspaceRoot: a.workspace.Root(), DataDirectory: a.config.DataDirectory,
		ValidateOwner: leaseOwnerValidator(a.repositories),
	})
	if err != nil {
		return err
	}
	a.workspace.AttachLeaseRepository(a.domain.Leases())
	a.workspace.AttachDescendantLiveness(a.domain)
	a.workspace.AttachWorkspaceViewRepository(a.repositories)
	worktrees, err := workspacecoord.NewWorktreeManager(a.workspace.Root())
	if err != nil {
		return err
	}
	a.coordinator, err = workspacecoord.NewCoordinator(a.repositories, worktrees)
	if err != nil {
		return err
	}
	a.artifacts, err = artifact.NewStore(filepath.Join(a.config.DataDirectory, "artifacts"))
	if err != nil {
		return err
	}
	a.workspace.AttachDiffArtifactStore(a.artifacts)
	a.coordinator.Artifacts, a.coordinator.Events = a.artifacts, a.repositories
	views := filepath.Join(a.config.DataDirectory, "views")
	if err := a.workspace.AttachSnapshotViewsDirectory(views); err != nil {
		return err
	}
	a.coordinator.Snapshots = &workspacecoord.SnapshotStore{
		ViewsDirectory: views, WorkspaceRoot: a.workspace.Root(),
		CopyLimit: a.config.Agents.SnapshotCopyLimit, Barrier: snapshotBarrier(a.domain.Leases()),
	}
	return nil
}

// resumeControlMaintenance completes prepared closures under the target's
// Session lock before Supervisor recovery can make its consumers runnable.
// The caller holds stateMu and has installed writable authority repositories.
func (a *App) resumeControlMaintenance(ctx context.Context, teamID orchestration.TeamID) error {
	root, err := a.existingControlRoot(ctx, teamID)
	if err != nil {
		return err
	}
	if _, owned := a.ownedTeams[teamID]; !owned || a.sessionOwners[root.SessionID] == nil {
		return errors.New("control maintenance requires ownership of the target Session")
	}
	return a.resumeOwnedSessionMaintenance(ctx, root.SessionID)
}

// Control may release its target Agents' leases, but must not sweep leases of
// unrelated Teams. Domain liveness remains responsible for abandoned holders.
type controlLeases struct{ orchestration.LeaseRepository }

func (controlLeases) ReapAbandonedLeases(ctx context.Context) (int, error) {
	return 0, ctx.Err()
}

// A rejecting pending store makes accidental writes fail before submission.
// Authority reads do not need a local SQLite database or pending replay.
type controlReadOnlyPending struct{}

func (controlReadOnlyPending) Insert(context.Context, aterequest.Record) error {
	return ateauthority.ErrReadOnly
}
func (controlReadOnlyPending) Get(context.Context, api.RequestKey) (aterequest.Record, bool, error) {
	return aterequest.Record{}, false, ateauthority.ErrReadOnly
}
func (controlReadOnlyPending) List(context.Context, aterequest.ListOptions) ([]aterequest.Record, error) {
	return nil, ateauthority.ErrReadOnly
}
func (controlReadOnlyPending) BeginAttempt(context.Context, aterequest.Record) (bool, error) {
	return false, ateauthority.ErrReadOnly
}
func (controlReadOnlyPending) DeleteAcknowledged(context.Context, aterequest.Record, api.CommitResult) error {
	return ateauthority.ErrReadOnly
}
func (controlReadOnlyPending) DeleteRejected(context.Context, aterequest.Record, api.CommitResult) error {
	return ateauthority.ErrReadOnly
}

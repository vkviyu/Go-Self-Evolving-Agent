// Package agentmanager owns the durable Multi-Agent lifecycle control plane.
// It never parses provider messages and never executes tools directly.
package agentmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentprofile"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/scheduler"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

var (
	ErrSupervisorClosed       = errors.New("Agent supervisor is closed")
	ErrAgentTerminal          = errors.New("Agent is already terminal")
	ErrWaitInput              = errors.New("wait requires at least one Agent ID")
	ErrSupervisorCloseTimeout = errors.New("Agent supervisor workers did not stop before close deadline")
)

// WaitSuspendedError is kept as a package alias for callers that already use
// the Supervisor package; the domain type is the adapter-safe contract.
type WaitSuspendedError = orchestration.WaitSuspendedError

type Runner interface {
	Run(ctx context.Context, request RunRequest) (orchestration.AgentResult, error)
}

// RunnerFunc adapts a function to Runner for composition-root wiring and tests.
type RunnerFunc func(context.Context, RunRequest) (orchestration.AgentResult, error)

func (runner RunnerFunc) Run(ctx context.Context, request RunRequest) (orchestration.AgentResult, error) {
	if runner == nil {
		return orchestration.AgentResult{}, errors.New("nil Agent runner")
	}
	return runner(ctx, request)
}

// RunnerOutcome lets an execution adapter hand a durable suspension back to
// the Supervisor without keeping a worker goroutine or model slot blocked.
// Existing Runner implementations remain valid and simply return a result.
type RunnerOutcome struct {
	Result orchestration.AgentResult
	Wait   *orchestration.WaitSpec
}

// SuspendingRunner is an optional adapter contract for Eino integrations.
// Implementations should checkpoint/interrupt at a safe boundary and return a
// WaitSpec when an agent_wait/approval/mailbox operation needs to suspend.
type SuspendingRunner interface {
	RunWithSuspension(context.Context, RunRequest) (RunnerOutcome, error)
}

// SuspendingRunnerFunc adapts a function that can explicitly return a durable
// wait outcome while still satisfying the legacy Runner interface.
type SuspendingRunnerFunc func(context.Context, RunRequest) (RunnerOutcome, error)

func (runner SuspendingRunnerFunc) RunWithSuspension(ctx context.Context, request RunRequest) (RunnerOutcome, error) {
	if runner == nil {
		return RunnerOutcome{}, errors.New("nil suspending Agent runner")
	}
	return runner(ctx, request)
}

func (runner SuspendingRunnerFunc) Run(ctx context.Context, request RunRequest) (orchestration.AgentResult, error) {
	outcome, err := runner.RunWithSuspension(ctx, request)
	if err != nil {
		return orchestration.AgentResult{}, err
	}
	if outcome.Wait != nil {
		return orchestration.AgentResult{Status: orchestration.ResultCanceled, Error: outcome.Wait.Condition}, &orchestration.WaitSuspendedError{WaitSet: orchestration.WaitSet{Condition: outcome.Wait.Condition}}
	}
	return outcome.Result, nil
}

// ContextSeeder materializes parent context at spawn time. The returned text
// is persisted with the child, making retries and restarts deterministic.
type ContextSeeder interface {
	Seed(context.Context, orchestration.AgentSnapshot, string) (string, error)
}

type BoundedContextSeeder interface {
	SeedWithProfile(context.Context, orchestration.AgentSnapshot, string, agentprofile.Profile) (string, error)
}

type ContextSeederFunc func(context.Context, orchestration.AgentSnapshot, string) (string, error)

func (seeder ContextSeederFunc) Seed(ctx context.Context, parent orchestration.AgentSnapshot, policy string) (string, error) {
	if seeder == nil {
		return "", nil
	}
	return seeder(ctx, parent, policy)
}

// RunRequest contains the durable child snapshot and its task prompt.
type RunRequest struct {
	Agent         orchestration.AgentSnapshot
	Prompt        string
	Messages      []orchestration.Message
	ResumeWaitSet orchestration.WaitSetID
	ResumeToken   string
	// Recover selects the original run's durable turn, never a new prompt.
	Recover bool
	// Activity reports what the run is doing now, such as the tool it
	// calls. The report lives in memory and ends with the run.
	Activity func(string)
}

type ContinueCommand struct {
	AgentID        orchestration.AgentID
	Prompt         string
	CorrelationID  string
	IdempotencyKey string
}

// WorkspaceDefault selects the workspace granted to writable children whose
// profile does not pin one.
type WorkspaceDefault string

const (
	// WorkspaceDefaultAuto isolates writers in snapshots when the workspace
	// coordinator supports them, and shares the leased workspace otherwise.
	WorkspaceDefaultAuto WorkspaceDefault = "auto"
	// WorkspaceDefaultSnapshot always isolates writers in snapshots.
	WorkspaceDefaultSnapshot WorkspaceDefault = "snapshot"
	// WorkspaceDefaultReadonly narrows writers to read-only access.
	WorkspaceDefaultReadonly WorkspaceDefault = "readonly"
	// WorkspaceDefaultShared lets writers share the leased workspace.
	WorkspaceDefaultShared WorkspaceDefault = "shared"
)

type Config struct {
	Agents                  orchestration.AgentRepository
	Tasks                   orchestration.TaskRepository
	Profiles                *agentprofile.Registry
	Runner                  Runner
	ContextSeeder           ContextSeeder
	Mailbox                 orchestration.MailboxRepository
	WaitSets                orchestration.WaitRepository
	Leases                  orchestration.LeaseRepository
	WaitPollInterval        time.Duration
	ModelBucketLimits       map[string]int
	ModelBucketDefaultLimit int
	UsageLedger             tokenusage.Ledger
	Workspace               *workspacecoord.Coordinator
	MergeExecutor           *workspacecoord.MergeExecutor
	WorkspaceRoot           string
	// DefaultWorkspace is resolved once at construction. Children persist
	// the granted mode, so a later change never affects spawned children.
	DefaultWorkspace WorkspaceDefault
	MaxLiveAgents    int
	MaxRunningAgents int
	MaxDepth         int
	MaxChildren      int
	RunContext       context.Context
}

type Supervisor struct {
	settlement    sync.Mutex
	agents        orchestration.AgentRepository
	tasks         orchestration.TaskRepository
	profiles      *agentprofile.Registry
	runner        Runner
	seeder        ContextSeeder
	mailbox       orchestration.MailboxRepository
	waitSets      orchestration.WaitRepository
	leases        orchestration.LeaseRepository
	waitInterval  time.Duration
	buckets       *scheduler.BucketSet
	fairQueue     scheduler.WeightedRoundRobin
	usageLedger   tokenusage.Ledger
	workspace     *workspacecoord.Coordinator
	mergeExecutor *workspacecoord.MergeExecutor
	workspaceRoot string
	// writerMode is the mode granted to writers without a pinned profile.
	writerMode  orchestration.WorkspaceMode
	maxLive     int
	maxRunning  int
	maxDepth    int
	maxChildren int
	ctx         context.Context
	cancel      context.CancelFunc

	mu             sync.Mutex
	closed         bool
	closeOnce      sync.Once
	closeErr       error
	wake           chan struct{}
	running        map[orchestration.AgentID]context.CancelFunc
	runningBuckets map[orchestration.AgentID]string
	// sharedWriters holds running children that write the shared workspace;
	// at most one runs at a time.
	sharedWriters  map[orchestration.AgentID]struct{}
	workspaceViews map[orchestration.AgentID]orchestration.WorkspaceViewID
	// mergeRuns holds Teams with a background merge pass; true asks the
	// pass to run once more because new merges may have become ready.
	mergeRuns map[orchestration.TeamID]bool
	// liveActivity holds what each running child reported last.
	liveActivity map[orchestration.AgentID]string
	workers      sync.WaitGroup
	admission    sync.Mutex
	teams        map[orchestration.TeamID]struct{}
}

func NewSupervisor(config Config) (*Supervisor, error) {
	if config.Agents == nil {
		return nil, errors.New("Agent supervisor requires an Agent repository")
	}
	if config.MaxLiveAgents <= 0 {
		config.MaxLiveAgents = 16
	}
	if config.MaxRunningAgents <= 0 {
		config.MaxRunningAgents = 1
	}
	if config.MaxDepth <= 0 {
		config.MaxDepth = 3
	}
	if config.MaxChildren <= 0 {
		config.MaxChildren = 4
	}
	if config.WaitPollInterval <= 0 {
		config.WaitPollInterval = time.Second
	}
	if config.ModelBucketDefaultLimit <= 0 {
		config.ModelBucketDefaultLimit = config.MaxRunningAgents
	}
	writer, err := writerMode(config.DefaultWorkspace, config.Workspace)
	if err != nil {
		return nil, err
	}
	parent := config.RunContext
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	waitSets := config.WaitSets
	if waitSets == nil {
		waitSets, _ = config.Agents.(orchestration.WaitRepository)
	}
	supervisor := &Supervisor{
		agents: config.Agents, tasks: config.Tasks, profiles: config.Profiles, runner: config.Runner, seeder: config.ContextSeeder, mailbox: config.Mailbox,
		waitSets: waitSets, leases: config.Leases,
		maxLive: config.MaxLiveAgents, maxRunning: config.MaxRunningAgents, maxDepth: config.MaxDepth, maxChildren: config.MaxChildren,
		waitInterval: config.WaitPollInterval,
		buckets:      scheduler.NewBucketSet(config.ModelBucketDefaultLimit, config.ModelBucketLimits),
		usageLedger:  config.UsageLedger,
		workspace:    config.Workspace, mergeExecutor: config.MergeExecutor, workspaceRoot: config.WorkspaceRoot, writerMode: writer,
		runningBuckets: make(map[orchestration.AgentID]string),
		sharedWriters:  make(map[orchestration.AgentID]struct{}),
		workspaceViews: make(map[orchestration.AgentID]orchestration.WorkspaceViewID),
		mergeRuns:      make(map[orchestration.TeamID]bool),
		liveActivity:   make(map[orchestration.AgentID]string),
		ctx:            ctx, cancel: cancel, wake: make(chan struct{}), running: make(map[orchestration.AgentID]context.CancelFunc), teams: make(map[orchestration.TeamID]struct{}),
	}
	if waitSets != nil || config.Runner != nil {
		supervisor.workers.Go(func() {
			supervisor.monitorWaitSets()
		})
	}
	return supervisor, nil
}

func (supervisor *Supervisor) Spawn(ctx context.Context, spec orchestration.SpawnSpec) (orchestration.AgentSnapshot, error) {
	if err := supervisor.ensureOpen(); err != nil {
		return orchestration.AgentSnapshot{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	supervisor.admission.Lock()
	defer supervisor.admission.Unlock()
	workspace, err := spec.Workspace.Normalize()
	if err != nil {
		return orchestration.AgentSnapshot{}, err
	}
	spec.Workspace = workspace
	if supervisor.profiles != nil && spec.ProfileName != "" {
		profiles := supervisor.profiles.Snapshot()
		profile, ok := profiles.Get(spec.ProfileName)
		if !ok {
			// Name the valid profiles so a model can correct its next call.
			return orchestration.AgentSnapshot{}, fmt.Errorf("%w: Agent profile %q (available: %s; omit profile to inherit the parent's)",
				orchestration.ErrNotFound, spec.ProfileName, strings.Join(profiles.Names(), ", "))
		}
		spec.ProfileRevision = profile.Revision
	}
	parent, err := supervisor.agents.GetAgent(ctx, spec.ParentAgentID)
	if err != nil {
		return orchestration.AgentSnapshot{}, err
	}
	if parent.TeamID != spec.TeamID {
		return orchestration.AgentSnapshot{}, fmt.Errorf("%w: parent Agent belongs to another team", orchestration.ErrInvalidEvent)
	}
	parentProfile, hasParentProfile := supervisor.agentProfile(parent)
	if !hasParentProfile && (supervisor.profiles != nil || len(parent.ProfileSnapshot) > 0) {
		return orchestration.AgentSnapshot{}, agentprofile.ErrSnapshot
	}
	if hasParentProfile && !parentProfile.CanSpawn {
		return orchestration.AgentSnapshot{}, fmt.Errorf("%w: parent Agent profile does not allow spawning children", orchestration.ErrInvalidEvent)
	}
	if spec.ProfileRevision == "" {
		if spec.ProfileName != "" && supervisor.profiles == nil {
			spec.ProfileRevision = spec.ProfileName
		} else {
			spec.ProfileRevision = parent.ProfileRevision
		}
	}
	var childProfile agentprofile.Profile
	if supervisor.profiles != nil {
		var ok bool
		childProfile, ok = supervisor.profileRevision(spec.ProfileRevision)
		if !ok {
			return orchestration.AgentSnapshot{}, fmt.Errorf("unknown Agent profile revision %q", spec.ProfileRevision)
		}
		if spec.Workspace == orchestration.WorkspaceReadonly || supervisor.writerMode == orchestration.WorkspaceModeSharedReadonly {
			// Narrowing is always permitted. It precedes the delegation check
			// so a read-only parent can spawn a read-only child of a
			// writable profile, and the runtime then denies write tools.
			childProfile.WorkspaceMode = agentprofile.WorkspaceSharedReadonly
		}
		if hasParentProfile {
			childProfile, err = agentprofile.ConstrainDelegation(parentProfile, childProfile)
			if err != nil {
				return orchestration.AgentSnapshot{}, err
			}
		}
		// Model/tool arguments never provide the trusted snapshot.
		spec.ProfileSnapshot, err = agentprofile.Encode(childProfile)
		if err != nil {
			return orchestration.AgentSnapshot{}, err
		}
		if spec.ContextPolicy == "" {
			spec.ContextPolicy = string(childProfile.ContextInheritance)
		}
		if inheritanceRank(spec.ContextPolicy) > inheritanceRank(string(childProfile.ContextInheritance)) {
			return orchestration.AgentSnapshot{}, errors.New("requested context inheritance exceeds Agent profile")
		}
	} else {
		spec.ProfileSnapshot = nil
	}
	// Resolve the granted access once; recovery reuses the persisted mode
	// instead of resolving the request again under changed defaults.
	spec.WorkspaceMode = grantWorkspaceMode(spec.Workspace, childProfile, supervisor.profiles != nil, supervisor.writerMode)
	if spec.ContextPolicy == "" {
		spec.ContextPolicy = string(agentprofile.InheritSummary)
	}
	if inheritanceRank(spec.ContextPolicy) < 0 {
		return orchestration.AgentSnapshot{}, fmt.Errorf("unsupported context policy %q", spec.ContextPolicy)
	}
	if supervisor.seeder != nil && spec.ContextPolicy != "none" {
		var inherited string
		var seedErr error
		if childProfile.Limits.MaxInheritedTokens > 0 {
			bounded, ok := supervisor.seeder.(BoundedContextSeeder)
			if !ok {
				return orchestration.AgentSnapshot{}, errors.New("context seeder cannot enforce profile token bound")
			}
			inherited, seedErr = bounded.SeedWithProfile(ctx, parent, spec.ContextPolicy, childProfile)
		} else {
			inherited, seedErr = supervisor.seeder.Seed(ctx, parent, spec.ContextPolicy)
		}
		if seedErr != nil {
			return orchestration.AgentSnapshot{}, fmt.Errorf("seed child context: %w", seedErr)
		}
		if strings.TrimSpace(inherited) != "" {
			spec.InitialPrompt = inherited + "\n\n" + spec.InitialPrompt
		}
	}
	if finder, ok := supervisor.agents.(orchestration.SpawnRequestFinder); ok && spec.IdempotencyKey != "" {
		existing, found, findErr := finder.FindSpawnRequest(ctx, spec)
		if findErr != nil {
			return orchestration.AgentSnapshot{}, findErr
		}
		if found {
			return existing, nil
		}
	}
	if parent.Depth+1 > supervisor.maxDepth {
		return orchestration.AgentSnapshot{}, fmt.Errorf("%w: maximum Agent depth is %d", orchestration.ErrAgentLimit, supervisor.maxDepth)
	}
	activeStates := activeAgentStates()
	agents, err := supervisor.agents.ListAgents(ctx, orchestration.AgentQuery{TeamID: spec.TeamID, States: activeStates})
	if err != nil {
		return orchestration.AgentSnapshot{}, err
	}
	if len(agents) >= supervisor.maxLive {
		return orchestration.AgentSnapshot{}, fmt.Errorf("%w: maximum live Agents is %d", orchestration.ErrAgentLimit, supervisor.maxLive)
	}
	children, err := supervisor.agents.ListAgents(ctx, orchestration.AgentQuery{TeamID: spec.TeamID, ParentAgentID: spec.ParentAgentID, States: activeStates})
	if err != nil {
		return orchestration.AgentSnapshot{}, err
	}
	childLimit := supervisor.maxChildren
	if hasParentProfile && parentProfile.Limits.MaxChildren < childLimit {
		childLimit = parentProfile.Limits.MaxChildren
	}
	if len(children) >= childLimit {
		return orchestration.AgentSnapshot{}, fmt.Errorf("%w: maximum direct children is %d", orchestration.ErrAgentLimit, childLimit)
	}
	child, err := supervisor.agents.CreateChild(ctx, spec)
	if err != nil {
		return orchestration.AgentSnapshot{}, err
	}
	supervisor.rememberTeam(child.TeamID)
	supervisor.signal()
	supervisor.startAvailable()
	return child, nil
}

func (supervisor *Supervisor) profileRevision(revision string) (agentprofile.Profile, bool) {
	if supervisor.profiles == nil {
		return agentprofile.Profile{}, false
	}
	return supervisor.profiles.Snapshot().GetRevision(revision)
}

// Continue starts another run on the same durable child thread. The directive
// is persisted before the state transition, so a crash cannot lose the stage
// input; an idempotency key makes retrying the pair safe.
func (supervisor *Supervisor) Continue(ctx context.Context, command ContinueCommand) (orchestration.AgentSnapshot, error) {
	if err := supervisor.ensureOpen(); err != nil {
		return orchestration.AgentSnapshot{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(command.Prompt) == "" {
		return orchestration.AgentSnapshot{}, errors.New("Agent continuation prompt is required")
	}
	// Wait observes the terminal projection before its post-commit effects
	// necessarily finish. Serialize continuation with that settlement window;
	// the repository still rejects an earlier Run whose effects remain pending.
	supervisor.settlement.Lock()
	defer supervisor.settlement.Unlock()
	supervisor.admission.Lock()
	defer supervisor.admission.Unlock()
	agent, err := supervisor.agents.GetAgent(ctx, command.AgentID)
	if err != nil {
		return orchestration.AgentSnapshot{}, err
	}
	if agent.ParentAgentID == "" {
		return orchestration.AgentSnapshot{}, errors.New("root Agent cannot be continued by the child-session API")
	}
	if !supervisor.teamAllowsExecution(ctx, agent.TeamID) {
		return orchestration.AgentSnapshot{}, orchestration.ErrTeamNotActive
	}
	if agent.AssignedTaskID != "" {
		return orchestration.AgentSnapshot{}, errors.New("task-assigned Agent cannot be reused as a Flow session")
	}
	if err := supervisor.ensureContinuableWorkspace(ctx, agent); err != nil {
		return orchestration.AgentSnapshot{}, err
	}
	if supervisor.mailbox == nil {
		return orchestration.AgentSnapshot{}, errors.New("Agent continuation requires a mailbox repository")
	}
	messageKey := command.IdempotencyKey
	if messageKey == "" {
		if command.CorrelationID != "" {
			messageKey = "continue:" + command.CorrelationID
		} else {
			// A direct library caller without a correlation or idempotency key is
			// asking for a new continuation. Never collapse all such requests
			// onto the literal "continue:" key.
			messageID, idErr := orchestration.NewMessageID()
			if idErr != nil {
				return orchestration.AgentSnapshot{}, fmt.Errorf("generate continuation request key: %w", idErr)
			}
			messageKey = "continue:" + string(messageID)
		}
	}
	body, err := json.Marshal(map[string]string{"prompt": command.Prompt})
	if err != nil {
		return orchestration.AgentSnapshot{}, err
	}
	messageSpec := orchestration.MessageSpec{
		TeamID: agent.TeamID, SenderAgentID: agent.ParentAgentID, RecipientAgentID: agent.ID,
		Type: orchestration.MessageControl, Body: body, CorrelationID: command.CorrelationID,
		IdempotencyKey: messageKey,
	}
	if agent.State != orchestration.AgentCompleted && agent.State != orchestration.AgentFailed {
		finder, ok := supervisor.mailbox.(orchestration.MessageRequestFinder)
		if !ok {
			return orchestration.AgentSnapshot{}, fmt.Errorf("Agent %s cannot continue from state %s", agent.ID, agent.State)
		}
		existing, found, findErr := finder.FindMessage(ctx, agent.TeamID, messageKey)
		if findErr != nil {
			return orchestration.AgentSnapshot{}, findErr
		}
		if !found || existing.RecipientAgentID != agent.ID || existing.SenderAgentID != agent.ParentAgentID || existing.CorrelationID != command.CorrelationID || string(existing.Body) != string(body) {
			return orchestration.AgentSnapshot{}, fmt.Errorf("Agent %s cannot continue from state %s", agent.ID, agent.State)
		}
		return agent, nil
	}
	if _, err := supervisor.mailbox.Enqueue(ctx, messageSpec); err != nil {
		return orchestration.AgentSnapshot{}, err
	}
	queued, err := supervisor.agents.TransitionAgent(ctx, orchestration.AgentTransition{
		AgentID: agent.ID, Expected: agent.Version, Target: orchestration.AgentQueued,
		Activity: "continued", CorrelationID: command.CorrelationID, Reason: "stateful Agent session continuation",
	})
	if err != nil {
		return orchestration.AgentSnapshot{}, err
	}
	supervisor.signal()
	supervisor.startAvailable()
	return queued, nil
}

// ResumeAgent releases an explicit external-recovery pause. It preserves the
// original RunID and Task claim, so resuming cannot create a second attempt.
func (supervisor *Supervisor) ResumeAgent(ctx context.Context, agentID orchestration.AgentID) (orchestration.AgentSnapshot, error) {
	if err := supervisor.ensureOpen(); err != nil {
		return orchestration.AgentSnapshot{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	agent, err := supervisor.agents.GetAgent(ctx, agentID)
	if err != nil {
		return orchestration.AgentSnapshot{}, err
	}
	if agent.State != orchestration.AgentWaiting || !strings.HasPrefix(agent.Activity, "recovery:") {
		return orchestration.AgentSnapshot{}, fmt.Errorf("Agent %s is not paused for external recovery", agentID)
	}
	if !supervisor.teamAllowsExecution(ctx, agent.TeamID) {
		return orchestration.AgentSnapshot{}, orchestration.ErrTeamNotActive
	}
	queued, err := supervisor.agents.TransitionAgent(ctx, orchestration.AgentTransition{
		AgentID: agent.ID, Expected: agent.Version, Target: orchestration.AgentQueued,
		RunID: agent.CurrentRunID, Activity: "recovery:resumed", Reason: "external recovery condition repaired",
	})
	if err != nil {
		return orchestration.AgentSnapshot{}, err
	}
	supervisor.signal()
	supervisor.startAvailable()
	return queued, nil
}

func (supervisor *Supervisor) Interrupt(ctx context.Context, agentID orchestration.AgentID) error {
	if err := supervisor.ensureOpen(); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	agent, err := supervisor.agents.GetAgent(ctx, agentID)
	if err != nil {
		return err
	}
	if isTerminal(agent.State) {
		return fmt.Errorf("%w: %s", ErrAgentTerminal, agentID)
	}
	if agent.State != orchestration.AgentCanceling {
		agent, err = supervisor.agents.TransitionAgent(ctx, orchestration.AgentTransition{
			AgentID: agent.ID, Expected: agent.Version, Target: orchestration.AgentCanceling, Activity: "canceling", Reason: "interrupt requested",
		})
		if err != nil {
			return err
		}
	}
	if supervisor.waitSets != nil {
		if err := supervisor.waitSets.CancelWaitSetsForAgent(ctx, agent.ID, "Agent interrupt requested"); err != nil {
			return err
		}
	}
	// Descendants stop with their parent, so no child keeps writing for a
	// run that was interrupted.
	err = supervisor.interruptChildren(ctx, agent)
	supervisor.mu.Lock()
	cancel := supervisor.running[agentID]
	supervisor.mu.Unlock()
	if cancel != nil {
		cancel()
	} else {
		canceled, cancelErr := supervisor.agents.TransitionAgent(ctx, orchestration.AgentTransition{
			AgentID: agent.ID, Expected: agent.Version, Target: orchestration.AgentCanceled, Activity: "canceled", Reason: "interrupt completed",
		})
		err = errors.Join(err, cancelErr)
		if cancelErr == nil {
			err = errors.Join(err, supervisor.releaseAssignedTask(ctx, canceled), supervisor.cleanupTerminalResources(ctx, canceled))
		}
	}
	supervisor.signal()
	return err
}

// interruptChildren interrupts the live children of agent, and through them
// every descendant. A child that finished meanwhile is skipped.
func (supervisor *Supervisor) interruptChildren(ctx context.Context, agent orchestration.AgentSnapshot) error {
	children, err := supervisor.agents.ListAgents(ctx, orchestration.AgentQuery{TeamID: agent.TeamID, ParentAgentID: agent.ID, States: activeAgentStates()})
	if err != nil {
		return fmt.Errorf("list children of Agent %s: %w", agent.ID, err)
	}
	var errs []error
	for _, child := range children {
		if err := supervisor.Interrupt(ctx, child.ID); err != nil && !errors.Is(err, ErrAgentTerminal) {
			errs = append(errs, fmt.Errorf("interrupt child Agent %s: %w", child.ID, err))
		}
	}
	return errors.Join(errs...)
}

// Close cancels and joins owned workers exactly once. All callers observe the
// same result, including a timeout: a second call is not proof of worker exit.
func (supervisor *Supervisor) Close() error {
	if supervisor == nil {
		return nil
	}
	supervisor.closeOnce.Do(func() { supervisor.closeErr = supervisor.close() })
	return supervisor.closeErr
}

func (supervisor *Supervisor) close() error {
	supervisor.mu.Lock()
	supervisor.closed = true
	teams := make([]orchestration.TeamID, 0, len(supervisor.teams))
	for teamID := range supervisor.teams {
		teams = append(teams, teamID)
	}
	for _, cancel := range supervisor.running {
		cancel()
	}
	supervisor.mu.Unlock()
	supervisor.cancel()
	supervisor.cancelDurableAgents(teams)
	workersDone := make(chan struct{})
	go func() {
		supervisor.workers.Wait()
		close(workersDone)
	}()
	var closeErr error
	workersStopped := false
	select {
	case <-workersDone:
		workersStopped = true
	case <-time.After(5 * time.Second):
		closeErr = ErrSupervisorCloseTimeout
	}
	// A runner that ignores cancellation can still mutate its workspace after
	// the close deadline. Keep durable leases in that case; releasing them
	// would let another process enter the same paths while the old worker is
	// still alive. Recovery/reaping can release the stale lease once ownership
	// is no longer live.
	if workersStopped && supervisor.leases != nil {
		if err := supervisor.releaseAllActiveLeases(teams); closeErr == nil && err != nil {
			closeErr = err
		}
	}
	supervisor.signal()
	return closeErr
}

func (supervisor *Supervisor) releaseAllActiveLeases(teams []orchestration.TeamID) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, teamID := range teams {
		leases, err := supervisor.leases.ListLeases(ctx, orchestration.LeaseQuery{TeamID: teamID, States: []orchestration.LeaseState{orchestration.LeaseActive}})
		if err != nil {
			return err
		}
		for _, lease := range leases {
			if _, err := supervisor.leases.ReleaseLease(ctx, lease.ID, lease.AgentID, "Supervisor closed"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (supervisor *Supervisor) cancelDurableAgents(teams []orchestration.TeamID) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, teamID := range teams {
		agents, err := supervisor.agents.ListAgents(ctx, orchestration.AgentQuery{TeamID: teamID, States: activeAgentStates()})
		if err != nil {
			continue
		}
		for _, agent := range agents {
			if agent.ParentAgentID == "" {
				continue
			}
			canceling := agent
			if agent.State != orchestration.AgentCanceling {
				canceling, err = supervisor.agents.TransitionAgent(ctx, orchestration.AgentTransition{
					AgentID: agent.ID, Expected: agent.Version, Target: orchestration.AgentCanceling,
					Activity: "canceling", Reason: "Supervisor closed",
				})
				if err != nil {
					continue
				}
			}
			if supervisor.waitSets != nil {
				_ = supervisor.waitSets.CancelWaitSetsForAgent(ctx, agent.ID, "Supervisor closed")
			}
			if !supervisor.hasRunner(agent.ID) {
				canceled, transitionErr := supervisor.agents.TransitionAgent(ctx, orchestration.AgentTransition{
					AgentID: canceling.ID, Expected: canceling.Version, Target: orchestration.AgentCanceled,
					Activity: "canceled", Reason: "Supervisor closed",
				})
				if transitionErr == nil {
					_ = supervisor.releaseAssignedTask(ctx, canceled)
					_ = supervisor.cleanupTerminalResources(ctx, canceled)
				}
			}
		}
	}
}

func (supervisor *Supervisor) signal() {
	supervisor.mu.Lock()
	old := supervisor.wake
	supervisor.wake = make(chan struct{})
	supervisor.mu.Unlock()
	close(old)
}

func (supervisor *Supervisor) ensureOpen() error {
	if supervisor == nil {
		return ErrSupervisorClosed
	}
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if supervisor.closed {
		return ErrSupervisorClosed
	}
	return nil
}

func (supervisor *Supervisor) hasRunner(agentID orchestration.AgentID) bool {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	_, ok := supervisor.running[agentID]
	return ok
}

func isTerminal(state orchestration.AgentState) bool {
	return state == orchestration.AgentCompleted || state == orchestration.AgentFailed || state == orchestration.AgentCanceled
}

func activeAgentStates() []orchestration.AgentState {
	return []orchestration.AgentState{
		orchestration.AgentCreated,
		orchestration.AgentQueued,
		orchestration.AgentRunning,
		orchestration.AgentWaiting,
		orchestration.AgentCanceling,
	}
}

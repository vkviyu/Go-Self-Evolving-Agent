package agentmanager

import (
	"context"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/scheduler"
)

// start launches agent when a running slot and its model bucket are free.
// Children that write the shared workspace run one at a time: leases only
// serialize single operations, so two such children could still interleave
// edits of one task. Read-only and isolated children share the other slots.
func (supervisor *Supervisor) start(agent orchestration.AgentSnapshot) {
	if !supervisor.backgroundEnabled() {
		return
	}
	if !supervisor.teamAllowsExecution(supervisor.ctx, agent.TeamID) {
		return
	}
	sharedWriter := supervisor.workspaceMode(agent) == orchestration.WorkspaceModeSharedLease
	runContext, cancel := context.WithCancel(supervisor.ctx)
	supervisor.mu.Lock()
	if supervisor.closed || supervisor.deferRecoveryDispatch || len(supervisor.running) >= supervisor.maxRunning {
		supervisor.mu.Unlock()
		cancel()
		return
	}
	if _, alreadyRunning := supervisor.running[agent.ID]; alreadyRunning {
		supervisor.mu.Unlock()
		cancel()
		return
	}
	if sharedWriter && len(supervisor.sharedWriters) > 0 {
		supervisor.mu.Unlock()
		cancel()
		return
	}
	bucketKey := supervisor.modelBucketKey(agent)
	if !supervisor.buckets.TryAcquire(bucketKey) {
		supervisor.mu.Unlock()
		cancel()
		return
	}
	supervisor.running[agent.ID] = cancel
	supervisor.runningBuckets[agent.ID] = bucketKey
	if sharedWriter {
		supervisor.sharedWriters[agent.ID] = struct{}{}
	}
	supervisor.workers.Add(1)
	supervisor.mu.Unlock()
	go func() {
		defer supervisor.workers.Done()
		supervisor.run(runContext, agent)
	}()
}

func (supervisor *Supervisor) startAvailable() {
	if supervisor.runner == nil || !supervisor.backgroundEnabled() {
		return
	}
	teams, err := supervisor.agents.ListTeams(supervisor.ctx)
	if err != nil {
		return
	}
	candidates := make([]orchestration.AgentSnapshot, 0)
	for _, teamID := range teams {
		if !supervisor.teamAllowsExecution(supervisor.ctx, teamID) {
			continue
		}
		if supervisor.usageLedger != nil {
			admissible, err := supervisor.usageLedger.CanAdmit(supervisor.ctx, teamID)
			if err != nil || !admissible {
				continue
			}
		}
		queued, err := supervisor.agents.ListAgents(supervisor.ctx, orchestration.AgentQuery{
			TeamID: teamID, States: []orchestration.AgentState{orchestration.AgentQueued},
		})
		if err != nil {
			continue
		}
		for _, agent := range queued {
			if supervisor.hasActiveParent(agent) {
				candidates = append(candidates, agent)
			}
		}
	}
	byID := make(map[string]orchestration.AgentSnapshot, len(candidates))
	ordered := make([]scheduler.Candidate, 0, len(candidates))
	for _, agent := range candidates {
		key := string(agent.ID)
		byID[key] = agent
		ordered = append(ordered, scheduler.Candidate{TeamKey: string(agent.TeamID), ItemKey: key})
	}
	for _, candidate := range supervisor.fairQueue.Order(ordered) {
		if !supervisor.hasRunCapacity() {
			return
		}
		supervisor.start(byID[candidate.ItemKey])
	}
}

// StartAvailableAsync resumes queued child Agents without blocking the command
// that is bringing the root conversation to its interactive surface.
func (supervisor *Supervisor) StartAvailableAsync() {
	done := make(chan struct{})
	close(done)
	supervisor.StartAvailableAfter(done)
}

// StartAvailableAfter resumes queued child Agents after a required startup
// operation completes. It also releases deferred monitoring and merge passes,
// after any in-progress Recover call finishes. Close cancels and joins the wait.
func (supervisor *Supervisor) StartAvailableAfter(done <-chan struct{}) {
	if supervisor == nil {
		return
	}
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if supervisor.closed || supervisor.ctx.Err() != nil {
		return
	}
	supervisor.workers.Go(func() {
		select {
		case <-done:
			supervisor.releaseStartup()
		case <-supervisor.ctx.Done():
		}
	})
}

func (supervisor *Supervisor) backgroundEnabled() bool {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	return !supervisor.closed && !supervisor.deferRecoveryDispatch && supervisor.ctx.Err() == nil
}

// startMonitorLocked is called during construction or with mu held.
func (supervisor *Supervisor) startMonitorLocked() {
	if supervisor.waitSets != nil || supervisor.runner != nil {
		supervisor.workers.Go(supervisor.monitorWaitSets)
	}
}

func (supervisor *Supervisor) releaseStartup() {
	select {
	case supervisor.recoveryGate <- struct{}{}:
		defer func() { <-supervisor.recoveryGate }()
	case <-supervisor.ctx.Done():
		return
	}
	supervisor.mu.Lock()
	if supervisor.closed || supervisor.ctx.Err() != nil {
		supervisor.mu.Unlock()
		return
	}
	var pending []orchestration.TeamID
	if supervisor.deferRecoveryDispatch {
		supervisor.deferRecoveryDispatch = false
		supervisor.startMonitorLocked()
		for teamID := range supervisor.pendingMerges {
			pending = append(pending, teamID)
		}
		clear(supervisor.pendingMerges)
	}
	supervisor.mu.Unlock()
	for _, teamID := range pending {
		supervisor.scheduleMerges(teamID)
	}
	supervisor.startAvailable()
}

// A queued Agent from before a Team cancellation must not be admitted again
// during recovery. CreateChild's transactional gate only protects new spawns,
// not existing queued or recovered runs.
func (supervisor *Supervisor) teamAllowsExecution(ctx context.Context, id orchestration.TeamID) bool {
	teams, ok := supervisor.agents.(orchestration.TeamRepository)
	if !ok {
		return true // older in-memory repository adapters have no Team lifecycle
	}
	team, err := teams.GetTeam(ctx, id)
	return err == nil && team.State == orchestration.TeamActive
}

func (supervisor *Supervisor) hasActiveParent(agent orchestration.AgentSnapshot) bool {
	if agent.ParentAgentID == "" {
		return false
	}
	parent, err := supervisor.agents.GetAgent(supervisor.ctx, agent.ParentAgentID)
	return err == nil && !isTerminal(parent.State) && parent.State != orchestration.AgentCanceling
}

func (supervisor *Supervisor) hasRunCapacity() bool {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	return !supervisor.closed && len(supervisor.running) < supervisor.maxRunning
}

func (supervisor *Supervisor) modelBucketKey(agent orchestration.AgentSnapshot) string {
	if profile, ok := supervisor.agentProfile(agent); ok && profile.ModelProfile != "" {
		return profile.ModelProfile
	}
	return "default"
}

func (supervisor *Supervisor) removeRunner(agentID orchestration.AgentID) {
	supervisor.mu.Lock()
	delete(supervisor.running, agentID)
	delete(supervisor.sharedWriters, agentID)
	delete(supervisor.liveActivity, agentID)
	bucketKey := supervisor.runningBuckets[agentID]
	delete(supervisor.runningBuckets, agentID)
	supervisor.mu.Unlock()
	if bucketKey != "" {
		supervisor.buckets.Release(bucketKey)
	}
}

// reportActivity records the live activity of a running child. A report
// that arrives after the run ended is dropped.
func (supervisor *Supervisor) reportActivity(agentID orchestration.AgentID, activity string) {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if _, running := supervisor.running[agentID]; !running {
		return
	}
	supervisor.liveActivity[agentID] = activity
}

// overlayActivity replaces the durable activity of running children with
// their live reports. A canceling child keeps its durable "canceling".
func (supervisor *Supervisor) overlayActivity(agents []orchestration.AgentSnapshot) {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	for index := range agents {
		if agents[index].State != orchestration.AgentRunning {
			continue
		}
		if activity, ok := supervisor.liveActivity[agents[index].ID]; ok && activity != "" {
			agents[index].Activity = activity
		}
	}
}

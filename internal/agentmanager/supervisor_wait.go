package agentmanager

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

type WaitCommand struct {
	AgentIDs []orchestration.AgentID
	Any      bool
	Timeout  time.Duration
}

type WaitResult struct {
	Agents    []orchestration.AgentSnapshot `json:"agents"`
	Satisfied bool                          `json:"satisfied"`
	TimedOut  bool                          `json:"timed_out"`
}

func (supervisor *Supervisor) monitorWaitSets() {
	ticker := time.NewTicker(supervisor.waitInterval)
	defer ticker.Stop()
	for {
		select {
		case <-supervisor.ctx.Done():
			return
		case <-ticker.C:
			teams, err := supervisor.agents.ListTeams(supervisor.ctx)
			if err != nil {
				continue
			}
			for _, teamID := range teams {
				_ = supervisor.recoverPendingCompletions(supervisor.ctx, teamID)
				_, _ = supervisor.ReconcileWaitSets(supervisor.ctx, teamID)
			}
		}
	}
}

// Suspend persists a wait condition and moves the owning Agent out of the
// running queue. It is the boundary an execution adapter uses after creating
// a checkpoint; no process-local channel is part of the durable contract.
func (supervisor *Supervisor) Suspend(ctx context.Context, spec orchestration.WaitSpec) (orchestration.WaitSet, error) {
	if err := supervisor.ensureOpen(); err != nil {
		return orchestration.WaitSet{}, err
	}
	if supervisor.waitSets == nil {
		return orchestration.WaitSet{}, errors.New("wait set repository is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := spec.Validate(); err != nil {
		return orchestration.WaitSet{}, err
	}
	agent, err := supervisor.agents.GetAgent(ctx, spec.AgentID)
	if err != nil {
		return orchestration.WaitSet{}, err
	}
	if agent.TeamID != spec.TeamID {
		return orchestration.WaitSet{}, fmt.Errorf("%w: wait Agent belongs to another Team", orchestration.ErrInvalidEvent)
	}
	if spec.RunID == "" {
		spec.RunID = agent.CurrentRunID
	}
	waitSet, err := supervisor.waitSets.CreateWaitSet(ctx, spec)
	if err == nil {
		supervisor.signal()
	}
	return waitSet, err
}

// ResolveWaitSet releases a suspended Agent and admits it through the normal
// scheduler. Resolution is idempotent in the durable repository.
func (supervisor *Supervisor) ResolveWaitSet(ctx context.Context, waitID orchestration.WaitSetID, reason string) (orchestration.WaitSet, error) {
	if err := supervisor.ensureOpen(); err != nil {
		return orchestration.WaitSet{}, err
	}
	if supervisor.waitSets == nil {
		return orchestration.WaitSet{}, errors.New("wait set repository is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	waitSet, err := supervisor.waitSets.ResolveWaitSet(ctx, waitID, reason)
	if err == nil {
		supervisor.signal()
		supervisor.startAvailable()
	}
	return waitSet, err
}

// ReconcileWaitSets evaluates pending durable waits. It is called after
// child state changes and during recovery; polling is only a latency aid, not
// the source of truth.
func (supervisor *Supervisor) ReconcileWaitSets(ctx context.Context, teamID orchestration.TeamID) (int, error) {
	if err := supervisor.ensureOpen(); err != nil {
		return 0, err
	}
	if supervisor.waitSets == nil {
		return 0, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := teamID.Validate(); err != nil {
		return 0, err
	}
	waits, err := supervisor.waitSets.ListWaitSets(ctx, orchestration.WaitSetQuery{TeamID: teamID, States: []orchestration.WaitSetState{orchestration.WaitSetPending}})
	if err != nil {
		return 0, err
	}
	resolved := 0
	now := time.Now().UTC()
	for _, waitSet := range waits {
		if waitSet.Deadline != nil && !now.Before(*waitSet.Deadline) {
			if _, err := supervisor.waitSets.ExpireWaitSet(ctx, waitSet.ID, "wait deadline reached"); err != nil {
				return resolved, err
			}
			resolved++
			continue
		}
		agents, err := supervisor.lookupAgents(ctx, waitSet.TargetAgentIDs)
		if err != nil {
			return resolved, err
		}
		if !waitSatisfied(agents, waitSet.Any) {
			continue
		}
		if _, err := supervisor.waitSets.ResolveWaitSet(ctx, waitSet.ID, "wait condition satisfied"); err != nil {
			return resolved, err
		}
		resolved++
	}
	if resolved > 0 {
		supervisor.signal()
		supervisor.startAvailable()
	}
	return resolved, nil
}

func (supervisor *Supervisor) Wait(ctx context.Context, command WaitCommand) (WaitResult, error) {
	if err := supervisor.ensureOpen(); err != nil {
		return WaitResult{}, err
	}
	if len(command.AgentIDs) == 0 {
		return WaitResult{}, ErrWaitInput
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var timer *time.Timer
	if command.Timeout > 0 {
		timer = time.NewTimer(command.Timeout)
		defer timer.Stop()
	}
	for {
		agents, err := supervisor.lookupAgents(ctx, command.AgentIDs)
		if err != nil {
			return WaitResult{}, err
		}
		if waitSatisfied(agents, command.Any) {
			return WaitResult{Agents: agents, Satisfied: true}, nil
		}
		supervisor.mu.Lock()
		wake := supervisor.wake
		supervisor.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
			return WaitResult{Agents: agents}, ctx.Err()
		case <-timerChannel(timer):
			return WaitResult{Agents: agents, TimedOut: true}, nil
		}
	}
}

func (supervisor *Supervisor) lookupAgents(ctx context.Context, ids []orchestration.AgentID) ([]orchestration.AgentSnapshot, error) {
	agents := make([]orchestration.AgentSnapshot, 0, len(ids))
	for _, id := range ids {
		agent, err := supervisor.agents.GetAgent(ctx, id)
		if err != nil {
			return nil, err
		}
		agents = append(agents, agent)
	}
	return agents, nil
}

func waitSatisfied(agents []orchestration.AgentSnapshot, any bool) bool {
	terminal := 0
	for _, agent := range agents {
		if isTerminal(agent.State) {
			terminal++
		}
	}
	if any {
		return terminal > 0
	}
	return terminal == len(agents)
}

func timerChannel(timer *time.Timer) <-chan time.Time {
	if timer == nil {
		return nil
	}
	return timer.C
}

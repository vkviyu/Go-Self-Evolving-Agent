package agentmanager

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

// Recover reconciles durable non-terminal children after process restart.
// Running and waiting states have no live worker in a new process, so they are
// safely returned to queued before admission. The root projection is ignored;
// the regular chat Conversation owns that Agent's execution.
func (supervisor *Supervisor) Recover(ctx context.Context) error {
	if err := supervisor.ensureOpen(); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// Releasing startup must not race a synchronous recovery still using a
	// storage batch. Ordinary recovery callers remain serialized as well.
	select {
	case supervisor.recoveryGate <- struct{}{}:
		defer func() { <-supervisor.recoveryGate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := supervisor.ensureOpen(); err != nil {
		return err
	}
	if ledger, ok := supervisor.usageLedger.(tokenusage.DurableReservationLedger); ok {
		if _, err := ledger.RecoverReservations(ctx); err != nil {
			return fmt.Errorf("reconcile model reservations during recovery: %w", err)
		}
	}
	if err := supervisor.recoverLeases(ctx); err != nil {
		return err
	}
	teams, err := supervisor.agents.ListTeams(ctx)
	if err != nil {
		return err
	}
	for _, teamID := range teams {
		supervisor.rememberTeam(teamID)
		if err := supervisor.recoverPendingCompletions(ctx, teamID); err != nil {
			return err
		}
		pendingRuns := make(map[orchestration.AgentID]orchestration.RunID)
		if receipts, ok := supervisor.agents.(orchestration.AgentCompletionRepository); ok {
			pending, err := receipts.PendingAgentCompletions(ctx, teamID)
			if err != nil {
				return err
			}
			for _, completion := range pending {
				pendingRuns[completion.AgentID] = completion.RunID
			}
		}
		if failed, listErr := supervisor.agents.ListAgents(ctx, orchestration.AgentQuery{TeamID: teamID, States: []orchestration.AgentState{orchestration.AgentFailed}}); listErr != nil {
			return listErr
		} else {
			for _, agent := range failed {
				supervisor.reassignRetryableTask(ctx, agent, "recovered retryable Agent failure")
			}
		}
		agents, err := supervisor.agents.ListAgents(ctx, orchestration.AgentQuery{TeamID: teamID, States: activeAgentStates()})
		if err != nil {
			return err
		}
		for _, agent := range agents {
			if agent.ParentAgentID == "" {
				continue
			}
			supervisor.mu.Lock()
			_, executing := supervisor.running[agent.ID]
			supervisor.mu.Unlock()
			if executing {
				continue
			}
			parent, parentErr := supervisor.agents.GetAgent(ctx, agent.ParentAgentID)
			if parentErr != nil {
				return parentErr
			}
			if isTerminal(parent.State) || parent.State == orchestration.AgentCanceling {
				// Also repair explicit cancellation interrupted before recursive
				// child cleanup; those terminal transitions may have no receipt.
				if err := supervisor.Interrupt(ctx, agent.ID); err != nil && !errors.Is(err, ErrAgentTerminal) {
					return err
				}
				continue
			}
			if run, pending := pendingRuns[agent.ID]; pending && run == agent.CurrentRunID {
				continue // worker returned; only coordinator settlement may retry
			}
			switch agent.State {
			case orchestration.AgentRunning:
				agent, err = supervisor.agents.TransitionAgent(ctx, orchestration.AgentTransition{
					AgentID: agent.ID, Expected: agent.Version, Target: orchestration.AgentQueued,
					Activity: "recovered", Reason: "Supervisor restarted",
				})
				if err != nil {
					return err
				}
			case orchestration.AgentWaiting:
				if strings.HasPrefix(agent.Activity, "recovery:") {
					// External recovery pauses are explicit. A process restart
					// must not spend another request automatically.
					continue
				}
				pending, err := supervisor.hasPendingWaitSet(ctx, agent)
				if err != nil {
					return err
				}
				if !pending {
					agent, err = supervisor.agents.TransitionAgent(ctx, orchestration.AgentTransition{
						AgentID: agent.ID, Expected: agent.Version, Target: orchestration.AgentQueued,
						Activity: "recovered", Reason: "wait condition was not durable",
					})
					if err != nil {
						return err
					}
				}
			case orchestration.AgentCanceling:
				agent, err = supervisor.agents.TransitionAgent(ctx, orchestration.AgentTransition{
					AgentID: agent.ID, Expected: agent.Version, Target: orchestration.AgentCanceled,
					Activity: "canceled", Reason: "Supervisor restarted during cancellation",
				})
				if err != nil {
					return err
				}
			}
			if agent.State == orchestration.AgentCanceled {
				if supervisor.waitSets != nil {
					if err := supervisor.waitSets.CancelWaitSetsForAgent(ctx, agent.ID, "Agent cancellation recovered after process restart"); err != nil {
						return fmt.Errorf("cancel recovered Agent wait sets: %w", err)
					}
				}
				if err := supervisor.releaseAssignedTask(ctx, agent); err != nil {
					return err
				}
				if err := supervisor.cleanupTerminalResources(ctx, agent); err != nil {
					return fmt.Errorf("release recovered Agent resources: %w", err)
				}
			}
		}
		if supervisor.runner != nil && supervisor.tasks != nil {
			if _, err := supervisor.dispatchReadyTasks(ctx, teamID); err != nil {
				continue
			}
		}
		if supervisor.waitSets != nil {
			_, _ = supervisor.ReconcileWaitSets(ctx, teamID)
		}
		// Resume journaled commits and merges queued before the restart.
		supervisor.scheduleMerges(teamID)
	}
	supervisor.startAvailable()
	supervisor.signal()
	return nil
}

// recoverPendingCompletions finishes coordinator effects for a result frozen
// before a process stopped. The worker is never invoked again; RunID prevents
// a late result from being applied to a later continuation.
func (supervisor *Supervisor) recoverPendingCompletions(ctx context.Context, teamID orchestration.TeamID) error {
	store, ok := supervisor.agents.(orchestration.AgentCompletionRepository)
	if !ok {
		return nil
	}
	completions, err := store.PendingAgentCompletions(ctx, teamID)
	if err != nil {
		return err
	}
	for _, completion := range completions {
		agent, getErr := supervisor.agents.GetAgent(ctx, completion.AgentID)
		if getErr != nil {
			return getErr
		}
		if agent.CurrentRunID != completion.RunID {
			// A newer run owns the Agent. Keep the old receipt for audit and
			// avoid applying a stale result to it.
			continue
		}
		supervisor.mu.Lock()
		_, executing := supervisor.running[agent.ID]
		supervisor.mu.Unlock()
		if executing {
			continue
		}
		if err := supervisor.finishContext(ctx, agent, completion.Result, nil); err != nil {
			if errors.Is(err, orchestration.ErrTaskNotReady) {
				continue
			}
			return fmt.Errorf("settle recovered Agent %s: %w", agent.ID, err)
		}
	}
	return nil
}

func (supervisor *Supervisor) hasPendingWaitSet(ctx context.Context, agent orchestration.AgentSnapshot) (bool, error) {
	if supervisor.waitSets == nil {
		return false, nil
	}
	waits, err := supervisor.waitSets.ListWaitSets(ctx, orchestration.WaitSetQuery{
		TeamID: agent.TeamID, AgentID: agent.ID, States: []orchestration.WaitSetState{orchestration.WaitSetPending},
	})
	return len(waits) > 0, err
}

// recoverLeases reclaims leases whose holder instance has exited, including
// those of the previous instance of this Session. Leases of live instances,
// possibly other Sessions on the same workspace, stay untouched. Durable file
// contents remain the source of truth after a crash, so a future run must
// inspect them before making a new write.
func (supervisor *Supervisor) recoverLeases(ctx context.Context) error {
	if supervisor.leases == nil {
		return nil
	}
	if _, err := supervisor.leases.ReapAbandonedLeases(ctx); err != nil {
		return fmt.Errorf("reclaim abandoned workspace leases during recovery: %w", err)
	}
	return nil
}

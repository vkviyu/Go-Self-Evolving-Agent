package agentmanager

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

func (supervisor *Supervisor) run(ctx context.Context, agent orchestration.AgentSnapshot) {
	if len(agent.ProfileSnapshot) > 0 {
		profile, ok := supervisor.agentProfile(agent)
		if !ok {
			supervisor.finish(agent, orchestration.AgentResult{Status: orchestration.ResultFailed, Error: "invalid Agent profile snapshot"}, fmt.Errorf("invalid Agent profile snapshot"))
			return
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, agent.CreatedAt.Add(profile.Limits.MaxWallTime))
		defer cancel()
	}
	continuation := agent.CurrentRunID != ""
	resuming := strings.HasPrefix(agent.Activity, "recovery:")
	recovering := agent.Activity == "recovered" || resuming
	resumeWait, err := supervisor.findResolvedWait(ctx, agent)
	if err != nil {
		supervisor.finish(agent, orchestration.AgentResult{Status: orchestration.ResultFailed, Error: err.Error()}, err)
		return
	}
	if resumeWait != nil && resumeWait.RunID != "" && resumeWait.RunID != agent.CurrentRunID {
		err := fmt.Errorf("resolved wait belongs to another Run ID")
		supervisor.finish(agent, orchestration.AgentResult{Status: orchestration.ResultFailed, Error: err.Error()}, err)
		return
	}
	runID := agent.CurrentRunID
	if !recovering && !resuming && resumeWait == nil {
		runID, err = orchestration.NewRunID()
	} else if runID == "" {
		err = fmt.Errorf("cannot recover Agent without original Run ID")
	}
	if err != nil {
		supervisor.finish(agent, orchestration.AgentResult{Status: orchestration.ResultFailed, Error: err.Error()}, err)
		return
	}
	transitionContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	// Team cancellation uses the same admission lock. It either observes this
	// run as running and interrupts it, or closes admission before any runner
	// can start spending its model/tool budget.
	supervisor.admission.Lock()
	if !supervisor.teamAllowsExecution(transitionContext, agent.TeamID) {
		supervisor.admission.Unlock()
		cancel()
		supervisor.removeRunner(agent.ID)
		supervisor.signal()
		return
	}
	running, err := supervisor.agents.TransitionAgent(transitionContext, orchestration.AgentTransition{
		AgentID: agent.ID, Expected: agent.Version, Target: orchestration.AgentRunning, RunID: runID, Activity: "runner",
	})
	supervisor.admission.Unlock()
	cancel()
	if err != nil {
		supervisor.removeRunner(agent.ID)
		supervisor.signal()
		return
	}
	if err := supervisor.startAssignedTask(ctx, running); err != nil {
		supervisor.finish(running, orchestration.AgentResult{Status: orchestration.ResultFailed, Error: err.Error()}, err)
		return
	}
	if err := supervisor.startWorkspaceView(ctx, running); err != nil {
		supervisor.finish(running, orchestration.AgentResult{Status: orchestration.ResultFailed, Error: err.Error()}, err)
		return
	}
	var messages []orchestration.Message
	if !recovering && resumeWait == nil {
		messages, err = supervisor.loadMessages(ctx, running.ID, running.TeamID)
		if err != nil {
			supervisor.finish(running, orchestration.AgentResult{Status: orchestration.ResultFailed, Error: err.Error()}, err)
			return
		}
	}
	prompt := running.InitialPrompt
	if continuation {
		prompt = ""
	}
	request := RunRequest{Agent: running, Prompt: prompt, Messages: messages, Recover: recovering}
	if resumeWait != nil {
		request.ResumeWaitSet = resumeWait.ID
		request.ResumeToken = resumeWait.ResumeToken
	}
	result, runErr, suspended := supervisor.executeRun(ctx, running, request)
	if suspended {
		return
	}
	// Only the Supervisor records deliveries.
	result.Delivery = nil
	classifyTaskAttemptFailure(&result, runErr)
	if runErr == nil && resumeWait != nil && supervisor.waitSets != nil {
		if _, err := supervisor.waitSets.ConsumeWaitSet(ctx, resumeWait.ID, "resume dispatched"); err != nil && runErr == nil {
			runErr = err
		}
	}
	// Recovery uses its saved input. Newly arrived mailbox data was not fed to
	// that checkpoint and must remain available for an explicit continuation.
	if runErr == nil && !recovering && resumeWait == nil {
		if err := supervisor.acknowledgeMessages(ctx, messages); err != nil {
			runErr = err
		}
	}
	if runErr != nil {
		result.Status = orchestration.ResultFailed
		result.Error = runErr.Error()
	}
	if err := result.Validate(); err != nil {
		// A malformed payload cannot be carried into the durable failure
		// receipt: the repository validates it again before freezing it.
		result = orchestration.AgentResult{Status: orchestration.ResultFailed, Summary: result.Summary, Error: err.Error()}
		runErr = err
	}
	if ctx.Err() != nil {
		result.Status = orchestration.ResultCanceled
		result.Retryable = false
		result.RetryCode = ""
		if result.Error == "" {
			result.Error = ctx.Err().Error()
		}
	}
	supervisor.finish(running, result, runErr)
}

func classifyTaskAttemptFailure(result *orchestration.AgentResult, err error) {
	// Never accept a runner/model supplied boolean on its own. Only a typed
	// failure from trusted runtime code enables repository safety inspection.
	result.Retryable, result.RetryCode = false, ""
	if _, paused := orchestration.ExecutionStopKindOf(err); paused {
		return
	}
	var failure *orchestration.TaskAttemptFailure
	if errors.As(err, &failure) && failure != nil && failure.Code.Retryable() && !result.OutcomeUnknown {
		result.Retryable, result.RetryCode = true, failure.Code
	}
}

func (supervisor *Supervisor) executeRun(ctx context.Context, agent orchestration.AgentSnapshot, request RunRequest) (orchestration.AgentResult, error, bool) {
	request.Activity = func(activity string) { supervisor.reportActivity(agent.ID, activity) }
	if suspending, ok := supervisor.runner.(SuspendingRunner); ok {
		outcome, err := suspending.RunWithSuspension(ctx, request)
		if err != nil || outcome.Wait == nil {
			return outcome.Result, err, false
		}
		waitSpec := *outcome.Wait
		waitSpec.TeamID = agent.TeamID
		waitSpec.AgentID = agent.ID
		if waitSpec.RunID == "" {
			waitSpec.RunID = agent.CurrentRunID
		}
		if waitSpec.CorrelationID == "" {
			waitSpec.CorrelationID = string(agent.CurrentRunID)
		}
		if _, err := supervisor.Suspend(ctx, waitSpec); err != nil {
			supervisor.finish(agent, orchestration.AgentResult{Status: orchestration.ResultFailed, Error: err.Error()}, err)
			return orchestration.AgentResult{}, err, true
		}
		supervisor.removeRunner(agent.ID)
		supervisor.signal()
		supervisor.startAvailable()
		return orchestration.AgentResult{}, nil, true
	}
	result, err := supervisor.runner.Run(ctx, request)
	return result, err, false
}

func (supervisor *Supervisor) findResolvedWait(ctx context.Context, agent orchestration.AgentSnapshot) (*orchestration.WaitSet, error) {
	if supervisor.waitSets == nil {
		return nil, nil
	}
	waits, err := supervisor.waitSets.ListWaitSets(ctx, orchestration.WaitSetQuery{
		TeamID: agent.TeamID, AgentID: agent.ID, States: []orchestration.WaitSetState{orchestration.WaitSetResolved, orchestration.WaitSetExpired},
	})
	if err != nil {
		return nil, fmt.Errorf("find resolved wait set: %w", err)
	}
	for _, waitSet := range slices.Backward(waits) {
		if waitSet.ResumeToken == "" {
			continue
		}

		return &waitSet, nil
	}
	return nil, nil
}

// finish freezes the worker outcome before coordinator effects. A settlement
// failure keeps that immutable receipt pending; it never rewrites a successful
// worker result into a new failure or runs the worker to retry coordination.
func (supervisor *Supervisor) finish(agent orchestration.AgentSnapshot, result orchestration.AgentResult, runErr error) error {
	supervisor.settlement.Lock()
	defer supervisor.settlement.Unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(supervisor.ctx), 5*time.Second)
	defer cancel()
	current, err := supervisor.agents.GetAgent(ctx, agent.ID)
	if err != nil {
		return err
	}
	if current.CurrentRunID != agent.CurrentRunID {
		return fmt.Errorf("%w: stale completion for Agent %s Run %s", orchestration.ErrAgentStateConflict, agent.ID, agent.CurrentRunID)
	}
	defer func() { supervisor.removeRunner(agent.ID); supervisor.signal(); supervisor.startAvailable() }()
	if kind, paused := orchestration.ExecutionStopKindOf(runErr); paused && current.State == orchestration.AgentRunning {
		_, err := supervisor.agents.TransitionAgent(ctx, orchestration.AgentTransition{
			AgentID: current.ID, Expected: current.Version, Target: orchestration.AgentWaiting,
			RunID: current.CurrentRunID, Activity: "recovery:" + string(kind), Reason: runErr.Error(),
		})
		return err
	}
	completionStore, durable := supervisor.agents.(orchestration.AgentCompletionRepository)
	// An already terminal Agent may still have an unfinished outbox, reviewer,
	// subtree cancellation or resource cleanup. Replaying these effects is safe.
	if isTerminal(current.State) {
		if current.Result != nil {
			result = *current.Result
		}
		if err := supervisor.afterAgentTransition(ctx, current, current.State, result); err != nil {
			return err
		}
		if durable && current.CurrentRunID != "" {
			return completionStore.SettleAgentCompletion(ctx, current.ID, current.CurrentRunID)
		}
		return nil
	}
	if durable && current.CurrentRunID != "" {
		if _, err := completionStore.PrepareAgentCompletion(ctx, orchestration.AgentCompletion{AgentID: current.ID, RunID: current.CurrentRunID, Result: result}); err != nil {
			return fmt.Errorf("freeze Agent completion: %w", err)
		}
	}
	target := terminalAgentState(current, result, runErr)
	if target == orchestration.AgentCanceled && current.State != orchestration.AgentCanceling {
		current, err = supervisor.prepareAgentCancellation(ctx, current, result.Error)
		if err != nil {
			return err
		}
	}
	if err := supervisor.finishAssignedTask(ctx, current, target, &result); err != nil {
		// The immutable pending receipt is the retry queue. Keep the execution
		// projection fenced to this Run, but release its process-local runner slot.
		// A returned parent waiting for refinement members must not keep path
		// leases that prevent those children from finishing. Its view stays
		// available for deterministic delivery and later settlement.
		if durable && errors.Is(err, orchestration.ErrTaskNotReady) {
			if releaseErr := supervisor.releaseAgentLeases(ctx, current); releaseErr != nil {
				return fmt.Errorf("release returned worker leases before settlement: %w", releaseErr)
			}
		}
		return err
	}
	updated, err := supervisor.agents.TransitionAgent(ctx, orchestration.AgentTransition{
		AgentID: current.ID, Expected: current.Version, Target: target, RunID: current.CurrentRunID,
		Activity: string(result.Status), Reason: result.Error, Result: &result,
	})
	if err != nil {
		return fmt.Errorf("commit Agent outcome: %w", err)
	}
	if err := supervisor.afterAgentTransition(ctx, updated, target, result); err != nil {
		return err
	}
	if durable && current.CurrentRunID != "" {
		return completionStore.SettleAgentCompletion(ctx, current.ID, current.CurrentRunID)
	}
	return nil
}

// A canceled runner may finish before Close has recorded canceling (or without
// Close, on a deadline). Preserve the existing state machine's intermediate
// state before committing its result. If Close/Interrupt wins this CAS, only
// accept that same Run's canceling snapshot; never overwrite a newer Run or
// another terminal outcome. Task/result effects are not retried.
func (supervisor *Supervisor) prepareAgentCancellation(ctx context.Context, current orchestration.AgentSnapshot, reason string) (orchestration.AgentSnapshot, error) {
	canceling, err := supervisor.agents.TransitionAgent(ctx, orchestration.AgentTransition{
		AgentID: current.ID, Expected: current.Version, Target: orchestration.AgentCanceling,
		Activity: "canceling", Reason: reason,
	})
	if errors.Is(err, orchestration.ErrAgentStateConflict) {
		latest, readErr := supervisor.agents.GetAgent(ctx, current.ID)
		if readErr == nil && latest.State == orchestration.AgentCanceling && latest.CurrentRunID == current.CurrentRunID {
			return latest, nil
		}
	}
	return canceling, err
}

func terminalAgentState(current orchestration.AgentSnapshot, result orchestration.AgentResult, runErr error) orchestration.AgentState {
	if result.Status == orchestration.ResultCanceled || current.State == orchestration.AgentCanceling {
		return orchestration.AgentCanceled
	}
	if result.Status == orchestration.ResultFailed || runErr != nil {
		return orchestration.AgentFailed
	}
	return orchestration.AgentCompleted
}

func (supervisor *Supervisor) afterAgentTransition(ctx context.Context, agent orchestration.AgentSnapshot, target orchestration.AgentState, result orchestration.AgentResult) error {
	if isTerminal(agent.State) && agent.ParentAgentID != "" {
		if err := supervisor.interruptChildren(ctx, agent); err != nil {
			return err
		}
	}
	if isTerminal(agent.State) && supervisor.mailbox != nil && agent.ParentAgentID != "" {
		spec, notify, err := orchestration.ResultMessage(agent)
		if err != nil {
			return err
		}
		if notify {
			if _, err := supervisor.mailbox.Enqueue(ctx, spec); err != nil {
				return err
			}
		}
	}
	if target == orchestration.AgentCompleted && supervisor.runner != nil && supervisor.tasks != nil {
		if agent.AssignedTaskID != "" {
			task, err := supervisor.tasks.GetTask(ctx, agent.AssignedTaskID)
			if err != nil {
				return err
			}
			if task.State == orchestration.TaskVerification {
				if _, err := supervisor.dispatchReviewer(ctx, agent, task); err != nil {
					return err
				}
			}
		}
		if _, err := supervisor.dispatchReadyTasks(ctx, agent.TeamID); err != nil {
			return err
		}
	}
	if supervisor.waitSets != nil {
		if _, err := supervisor.ReconcileWaitSets(ctx, agent.TeamID); err != nil {
			return err
		}
	}
	if isTerminal(agent.State) {
		if err := supervisor.cleanupTerminalResources(ctx, agent); err != nil {
			return err
		}
		if target == orchestration.AgentFailed && result.Retryable && !result.OutcomeUnknown {
			supervisor.reassignRetryableTask(ctx, agent, "trusted recoverable task attempt failure")
		}
		supervisor.scheduleMerges(agent.TeamID)
	}
	return nil
}

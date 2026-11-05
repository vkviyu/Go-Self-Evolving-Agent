package agentmanager

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

func (supervisor *Supervisor) authorizeTaskControl(ctx context.Context, actor orchestration.ActorScope) error {
	profile, ok, err := supervisor.actorProfile(ctx, actor)
	if err != nil {
		return err
	}
	if ok && !profile.CanManageTasks {
		return fmt.Errorf("%w: Agent profile does not allow task management", orchestration.ErrInvalidEvent)
	}
	return nil
}

func (supervisor *Supervisor) authorizeTaskReviewer(ctx context.Context, actor orchestration.ActorScope, task orchestration.TaskSnapshot) error {
	if task.VerificationProfile == "" {
		return nil
	}
	if supervisor.profiles == nil {
		if actor.ProfileRevision == task.VerificationProfile {
			return nil
		}
		return fmt.Errorf("%w: task %s requires reviewer profile %q", orchestration.ErrTaskReviewerRequired, task.ID, task.VerificationProfile)
	}
	profile, ok, err := supervisor.actorProfile(ctx, actor)
	if err != nil {
		return err
	}
	if !ok || (profile.Name != task.VerificationProfile && profile.Revision != task.VerificationProfile) {
		return fmt.Errorf("%w: task %s requires reviewer profile %q", orchestration.ErrTaskReviewerRequired, task.ID, task.VerificationProfile)
	}
	reviewer, found, err := supervisor.agents.FindSpawn(ctx, task.TeamID, reviewKey(task))
	if err != nil {
		return fmt.Errorf("resolve task reviewer: %w", err)
	}
	if !found || reviewer.ID != actor.AgentID || reviewer.ID == task.AssigneeAgentID {
		return fmt.Errorf("%w: Agent is not the designated reviewer of task %s attempt %d", orchestration.ErrTaskReviewerRequired, task.ID, task.Attempt)
	}
	return nil
}

func (supervisor *Supervisor) authorizeTaskMutation(ctx context.Context, actor orchestration.ActorScope, task orchestration.TaskSnapshot) error {
	if task.AssigneeAgentID == "" || task.AssigneeAgentID == actor.AgentID {
		return nil
	}
	if task.State == orchestration.TaskVerification && supervisor.authorizeTaskReviewer(ctx, actor, task) == nil {
		return nil
	}
	current, err := supervisor.agents.GetAgent(ctx, actor.AgentID)
	if err != nil {
		return err
	}
	if current.ParentAgentID == "" {
		return nil
	}
	return fmt.Errorf("%w: task is assigned to %s", orchestration.ErrInvalidEvent, task.AssigneeAgentID)
}

func (supervisor *Supervisor) CreateTask(ctx context.Context, spec orchestration.TaskSpec) (orchestration.TaskSnapshot, error) {
	if err := supervisor.ensureOpen(); err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	if supervisor.tasks == nil {
		return orchestration.TaskSnapshot{}, errors.New("task repository is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	task, err := supervisor.tasks.CreateTask(ctx, spec)
	if err == nil {
		supervisor.rememberTeam(task.TeamID)
		supervisor.signal()
	}
	return task, err
}

// RefineTasks commits an additive plan increment. The repository owns the
// transaction, Team state and cycle check; Supervisor enforces the actor's
// task-management capability boundary.
func (supervisor *Supervisor) RefineTasks(ctx context.Context, spec orchestration.TaskRefinementSpec) (orchestration.TaskRefinementResult, error) {
	if err := supervisor.ensureOpen(); err != nil {
		return orchestration.TaskRefinementResult{}, err
	}
	refiner, ok := supervisor.tasks.(orchestration.TaskRefinementRepository)
	if !ok {
		return orchestration.TaskRefinementResult{}, errors.New("task refinement is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := spec.Validate(); err != nil {
		return orchestration.TaskRefinementResult{}, err
	}
	actor, err := supervisor.agents.GetAgent(ctx, spec.ActorAgentID)
	if err != nil {
		return orchestration.TaskRefinementResult{}, err
	}
	if actor.TeamID != spec.TeamID {
		return orchestration.TaskRefinementResult{}, fmt.Errorf("%w: refinement actor belongs to another team", orchestration.ErrInvalidEvent)
	}
	if err := supervisor.authorizeTaskControl(ctx, actor.ActorScope()); err != nil {
		return orchestration.TaskRefinementResult{}, err
	}
	result, err := refiner.RefineTasks(ctx, spec)
	if err == nil {
		supervisor.rememberTeam(spec.TeamID)
		supervisor.signal()
	}
	return result, err
}

func (supervisor *Supervisor) UpdateTask(ctx context.Context, update orchestration.TaskUpdate) (orchestration.TaskSnapshot, error) {
	if err := supervisor.ensureOpen(); err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	if supervisor.tasks == nil {
		return orchestration.TaskSnapshot{}, errors.New("task repository is unavailable")
	}
	plans, ok := supervisor.tasks.(orchestration.TaskPlanRepository)
	if !ok {
		return orchestration.TaskSnapshot{}, errors.New("task plan editing is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	task, err := supervisor.tasks.GetTask(ctx, update.TaskID)
	if err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	if update.ActorAgentID != "" {
		actor, actorErr := supervisor.agents.GetAgent(ctx, update.ActorAgentID)
		if actorErr != nil {
			return orchestration.TaskSnapshot{}, actorErr
		}
		if actor.TeamID != task.TeamID {
			return orchestration.TaskSnapshot{}, fmt.Errorf("%w: task belongs to another Team", orchestration.ErrInvalidEvent)
		}
		if err := supervisor.authorizeTaskControl(ctx, actor.ActorScope()); err != nil {
			return orchestration.TaskSnapshot{}, err
		}
		// The plan repository authorizes current Task owners and their Agent
		// ancestors in the same transaction as the versioned metadata change.
	}
	updated, err := plans.UpdateTask(ctx, update)
	if err == nil {
		supervisor.rememberTeam(updated.TeamID)
		supervisor.signal()
	}
	return updated, err
}

func (supervisor *Supervisor) TransitionTask(ctx context.Context, transition orchestration.TaskTransition) (orchestration.TaskSnapshot, error) {
	if err := supervisor.ensureOpen(); err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	if supervisor.tasks == nil {
		return orchestration.TaskSnapshot{}, errors.New("task repository is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if transition.Target == orchestration.TaskCompleted || transition.Target == orchestration.TaskAwaitingMerge || transition.Target == orchestration.TaskReady {
		current, err := supervisor.tasks.GetTask(ctx, transition.TaskID)
		if err != nil {
			return orchestration.TaskSnapshot{}, err
		}
		if current.VerificationProfile != "" && current.State == orchestration.TaskVerification {
			if transition.ActorAgentID == "" {
				return orchestration.TaskSnapshot{}, orchestration.ErrTaskReviewerRequired
			}
			reviewer, err := supervisor.agents.GetAgent(ctx, transition.ActorAgentID)
			if err != nil {
				return orchestration.TaskSnapshot{}, err
			}
			if reviewer.ID == current.AssigneeAgentID {
				return orchestration.TaskSnapshot{}, fmt.Errorf("%w: worker %s cannot self-review task %s", orchestration.ErrTaskReviewerRequired, reviewer.ID, current.ID)
			}
			if reviewer.TeamID != current.TeamID {
				return orchestration.TaskSnapshot{}, fmt.Errorf("%w: reviewer belongs to another Team", orchestration.ErrInvalidEvent)
			}
			if err := supervisor.authorizeTaskReviewer(ctx, reviewer.ActorScope(), current); err != nil {
				return orchestration.TaskSnapshot{}, err
			}
			// Reviewer authorization is for this attempt and version, not any
			// later verification that happens to exist when the SQL write runs.
			if transition.Expected == orchestration.AnySequence {
				transition.Expected = current.Version
			}
		}
	}
	task, err := supervisor.tasks.TransitionTask(ctx, transition)
	if err == nil {
		supervisor.rememberTeam(task.TeamID)
		if supervisor.runner != nil && (task.State == orchestration.TaskReady || task.State == orchestration.TaskCompleted) {
			_, _ = supervisor.dispatchReadyTasks(ctx, task.TeamID)
		}
		supervisor.signal()
	}
	return task, err
}

func (supervisor *Supervisor) ClaimTask(ctx context.Context, taskID orchestration.TaskID, agentID orchestration.AgentID, expected uint64) (orchestration.TaskSnapshot, error) {
	if err := supervisor.ensureOpen(); err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	if supervisor.tasks == nil {
		return orchestration.TaskSnapshot{}, errors.New("task repository is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	task, err := supervisor.tasks.ClaimTask(ctx, taskID, agentID, expected)
	if err == nil {
		supervisor.signal()
	}
	return task, err
}

func (supervisor *Supervisor) ReleaseTask(ctx context.Context, taskID orchestration.TaskID, agentID orchestration.AgentID, expected uint64) (orchestration.TaskSnapshot, error) {
	if err := supervisor.ensureOpen(); err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	if supervisor.tasks == nil {
		return orchestration.TaskSnapshot{}, errors.New("task repository is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	task, err := supervisor.tasks.ReleaseTask(ctx, taskID, agentID, expected)
	if err == nil {
		supervisor.rememberTeam(task.TeamID)
		if supervisor.runner != nil {
			_, _ = supervisor.dispatchReadyTasks(ctx, task.TeamID)
		}
		supervisor.signal()
	}
	return task, err
}

// ReassignTask reopens only an explicitly retryable failed assignment. The
// repository verifies the frozen Agent result and increments the Task attempt
// atomically; dispatch uses the new attempt in its idempotency key.
func (supervisor *Supervisor) ReassignTask(ctx context.Context, taskID orchestration.TaskID, failedAgentID orchestration.AgentID, expected uint64, reason string) (orchestration.TaskSnapshot, error) {
	if err := supervisor.ensureOpen(); err != nil {
		return orchestration.TaskSnapshot{}, err
	}
	reassigner, ok := supervisor.tasks.(orchestration.TaskReassignmentRepository)
	if !ok {
		return orchestration.TaskSnapshot{}, errors.New("task repository does not support reassignment")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// Leases and workspace state live in the workspace domain, which may use
	// a different database from the Task repository. Check those authoritative
	// ports as well as the repository's transactional journal proof.
	supervisor.admission.Lock()
	if err := supervisor.proveReassignmentQuiescent(ctx, taskID, failedAgentID); err != nil {
		supervisor.admission.Unlock()
		return orchestration.TaskSnapshot{}, err
	}
	task, err := reassigner.ReassignTask(ctx, taskID, failedAgentID, expected, reason)
	supervisor.admission.Unlock()
	if err == nil {
		if supervisor.runner != nil {
			_, _ = supervisor.dispatchReadyTasks(ctx, task.TeamID)
		}
		supervisor.signal()
	}
	return task, err
}

func (supervisor *Supervisor) reassignRetryableTask(ctx context.Context, agent orchestration.AgentSnapshot, reason string) {
	if supervisor.tasks == nil || agent.AssignedTaskID == "" || agent.Result == nil || !agent.Result.Retryable || !agent.Result.RetryCode.Retryable() || agent.Result.OutcomeUnknown {
		return
	}
	task, err := supervisor.tasks.GetTask(ctx, agent.AssignedTaskID)
	if err != nil || task.State != orchestration.TaskFailed {
		return
	}
	if _, err := supervisor.ReassignTask(ctx, task.ID, agent.ID, task.Version, reason); err != nil {
		return
	}
}

// dispatchReadyTasks admits ready, unassigned tasks of a team as children of
// its root Agent, up to the live Agent limit. The repository claim is the
// concurrency boundary: if another scheduler wins a race, the losing dispatch
// simply leaves that task untouched.
func (supervisor *Supervisor) dispatchReadyTasks(ctx context.Context, teamID orchestration.TeamID) ([]orchestration.AgentSnapshot, error) {
	limit := supervisor.maxLive
	if reaper, ok := supervisor.tasks.(orchestration.TaskClaimReaper); ok {
		if _, err := reaper.ReapExpiredTaskClaims(ctx, time.Now().UTC()); err != nil {
			return nil, fmt.Errorf("reap expired task claims: %w", err)
		}
	}
	parent, err := supervisor.dispatchParent(ctx, teamID)
	if err != nil {
		return nil, err
	}
	tasks, err := supervisor.tasks.ListTasks(ctx, orchestration.TaskQuery{TeamID: teamID, States: []orchestration.TaskState{orchestration.TaskReady}})
	if err != nil {
		return nil, fmt.Errorf("list ready tasks: %w", err)
	}
	dispatched := make([]orchestration.AgentSnapshot, 0, min(limit, len(tasks)))
	var firstErr error
	for _, task := range tasks {
		if len(dispatched) >= limit {
			break
		}
		if task.AssigneeAgentID != "" {
			// A stale or concurrently claimed ready row must not prevent later
			// unassigned tasks from being admitted in the same scheduler pass.
			continue
		}
		prompt := task.Title
		if strings.TrimSpace(task.Description) != "" {
			prompt += "\n\n" + task.Description
		}
		child, spawnErr := supervisor.Spawn(ctx, orchestration.SpawnSpec{
			TeamID: teamID, ParentAgentID: parent.ID, TaskID: task.ID, TaskVersion: task.Version, ProfileName: task.RequiredProfile,
			InitialPrompt: prompt, IdempotencyKey: fmt.Sprintf("task-dispatch:%s:%d", task.ID, task.Attempt),
			CorrelationID: fmt.Sprintf("task:%s", task.ID),
		})
		if spawnErr != nil {
			if errors.Is(spawnErr, orchestration.ErrTaskClaimConflict) || errors.Is(spawnErr, orchestration.ErrTaskNotReady) || errors.Is(spawnErr, orchestration.ErrTaskStateConflict) {
				continue
			}
			if errors.Is(spawnErr, orchestration.ErrAgentLimit) {
				break
			}
			// A canceled Team admits no Agents; its ready tasks wait for the
			// session to reopen it.
			if errors.Is(spawnErr, orchestration.ErrTeamNotActive) {
				break
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("dispatch task %s: %w", task.ID, spawnErr)
			}
			continue
		}
		dispatched = append(dispatched, child)
	}
	return dispatched, firstErr
}

// dispatchParent selects the team's first active root Agent as the
// coordinator of dispatched tasks.
func (supervisor *Supervisor) dispatchParent(ctx context.Context, teamID orchestration.TeamID) (orchestration.AgentSnapshot, error) {
	parents, err := supervisor.agents.ListAgents(ctx, orchestration.AgentQuery{TeamID: teamID, ParentAgentID: ""})
	if err != nil {
		return orchestration.AgentSnapshot{}, fmt.Errorf("find dispatch parent: %w", err)
	}
	for _, parent := range parents {
		if !isTerminal(parent.State) && parent.State != orchestration.AgentCanceling {
			return parent, nil
		}
	}
	return orchestration.AgentSnapshot{}, fmt.Errorf("%w: no active root Agent for Team %s", orchestration.ErrNotFound, teamID)
}

// rememberTeam records a Team this supervisor touched, so Close can cancel
// its durable Agents.
func (supervisor *Supervisor) rememberTeam(teamID orchestration.TeamID) {
	supervisor.mu.Lock()
	supervisor.teams[teamID] = struct{}{}
	supervisor.mu.Unlock()
}

func (supervisor *Supervisor) AddTaskDependency(ctx context.Context, taskID, dependencyID orchestration.TaskID, actorAgentID orchestration.AgentID, correlationID string) error {
	if err := supervisor.ensureOpen(); err != nil {
		return err
	}
	if supervisor.tasks == nil {
		return errors.New("task repository is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	err := supervisor.tasks.AddDependency(ctx, taskID, dependencyID, actorAgentID, correlationID)
	if err == nil {
		supervisor.signal()
	}
	return err
}

func (supervisor *Supervisor) RemoveTaskDependency(ctx context.Context, taskID, dependencyID orchestration.TaskID, actorAgentID orchestration.AgentID, correlationID string) error {
	if err := supervisor.ensureOpen(); err != nil {
		return err
	}
	if supervisor.tasks == nil {
		return errors.New("task repository is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	err := supervisor.tasks.RemoveDependency(ctx, taskID, dependencyID, actorAgentID, correlationID)
	if err == nil {
		if supervisor.runner != nil {
			if task, getErr := supervisor.tasks.GetTask(ctx, taskID); getErr == nil && task.State == orchestration.TaskReady {
				_, _ = supervisor.dispatchReadyTasks(ctx, task.TeamID)
			}
		}
		supervisor.signal()
	}
	return err
}

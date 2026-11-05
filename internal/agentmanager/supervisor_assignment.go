package agentmanager

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

// dispatchReviewer spawns the reviewer of a task that names a verification
// profile, as a sibling of the worker.
func (supervisor *Supervisor) dispatchReviewer(ctx context.Context, worker orchestration.AgentSnapshot, task orchestration.TaskSnapshot) (orchestration.AgentSnapshot, error) {
	if task.VerificationProfile == "" {
		return orchestration.AgentSnapshot{}, nil
	}
	if worker.ParentAgentID == "" {
		return orchestration.AgentSnapshot{}, fmt.Errorf("task %s requires a reviewer parent", task.ID)
	}
	// Without a registry the verification profile is taken as a revision.
	profileName, profileRevision := "", task.VerificationProfile
	if supervisor.profiles != nil {
		snapshot := supervisor.profiles.Snapshot()
		profile, ok := snapshot.Get(task.VerificationProfile)
		if !ok {
			profile, ok = snapshot.GetRevision(task.VerificationProfile)
		}
		if !ok {
			return orchestration.AgentSnapshot{}, fmt.Errorf("%w: reviewer profile %q", orchestration.ErrNotFound, task.VerificationProfile)
		}
		profileName, profileRevision = profile.Name, profile.Revision
	}
	prompt := fmt.Sprintf("Review task %s: %s", task.ID, task.Title)
	if strings.TrimSpace(task.Description) != "" {
		prompt += "\n\n" + task.Description
	}
	prompt += "\n\nInspect the worker result and call task_verify with passed=true only when the acceptance criteria are met."
	prompt += supervisor.candidateReviewNote(ctx, task)
	return supervisor.Spawn(ctx, orchestration.SpawnSpec{
		TeamID: worker.TeamID, ParentAgentID: worker.ParentAgentID, ProfileName: profileName, ProfileRevision: profileRevision,
		InitialPrompt: prompt, IdempotencyKey: reviewKey(task), CorrelationID: fmt.Sprintf("task-review:%s", task.ID),
	})
}

// reviewKey is the spawn idempotency key of the reviewer of one task attempt.
func reviewKey(task orchestration.TaskSnapshot) string {
	return fmt.Sprintf("task-review:%s:%d", task.ID, task.Attempt)
}

// candidateReviewNote tells a reviewer which snapshot candidate or worktree
// patch it reviews and includes its diff. It is empty when the task has no
// pending merge; a missing diff only shortens the prompt.
func (supervisor *Supervisor) candidateReviewNote(ctx context.Context, task orchestration.TaskSnapshot) string {
	if supervisor.workspace == nil || supervisor.workspace.Repository == nil {
		return ""
	}
	merge, err := supervisor.latestMerge(ctx, workspacecoord.MergeQuery{
		TeamID: task.TeamID, TaskID: task.ID, States: []workspacecoord.MergeState{workspacecoord.MergePending},
	})
	if err != nil || merge.ID == "" {
		return ""
	}
	if merge.Kind != workspacecoord.MergeSnapshot {
		return supervisor.patchReviewNote(ctx, merge)
	}
	note := fmt.Sprintf("\n\nThe workspace shows snapshot candidate %s read-only; it reaches the main workspace only after your verification passes.", merge.CandidateDigest)
	store, err := supervisor.artifactStore()
	if err != nil {
		return note
	}
	candidate, err := workspacecoord.LoadCandidate(ctx, store, merge.PatchArtifact)
	if err != nil {
		return note
	}
	diff, err := workspacecoord.CandidateDiff(ctx, candidate, store)
	if err != nil || strings.TrimSpace(diff) == "" {
		return note
	}
	return note + "\n\nCandidate diff:\n```diff\n" + diff + "\n```"
}

// patchReviewNote tells a reviewer which worktree patch it reviews and
// includes the patch. A missing patch only shortens the prompt.
func (supervisor *Supervisor) patchReviewNote(ctx context.Context, merge workspacecoord.MergeRequest) string {
	note := fmt.Sprintf("\n\nThe workspace shows the git worktree of merge %s read-only; its patch reaches the main workspace only after your verification passes.", merge.ID)
	store, err := supervisor.artifactStore()
	if err != nil {
		return note
	}
	patch, err := store.ReadArtifact(ctx, merge.PatchArtifact)
	if err != nil || len(bytes.TrimSpace(patch)) == 0 {
		return note
	}
	return note + "\n\nPatch:\n```diff\n" + workspacecoord.PatchDiff(patch) + "\n```"
}

// startAssignedTask advances a claimed task when its child receives a run
// slot. Recovery may leave the task in progress, which is intentionally
// idempotent and lets the next run resume the same logical attempt.
func (supervisor *Supervisor) startAssignedTask(ctx context.Context, agent orchestration.AgentSnapshot) error {
	if supervisor.tasks == nil || agent.AssignedTaskID == "" {
		return nil
	}
	task, err := supervisor.tasks.GetTask(ctx, agent.AssignedTaskID)
	if err != nil {
		return fmt.Errorf("load assigned task: %w", err)
	}
	if err := checkTaskOwner(task, agent); err != nil {
		return err
	}
	switch task.State {
	case orchestration.TaskClaimed:
		_, err = supervisor.tasks.TransitionTask(ctx, orchestration.TaskTransition{
			TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskInProgress,
			AssigneeAgentID: agent.ID, ActorAgentID: agent.ID, CorrelationID: string(agent.CurrentRunID), Reason: "Agent run started",
		})
		if err != nil {
			return fmt.Errorf("start assigned task: %w", err)
		}
	case orchestration.TaskInProgress:
		return nil
	case orchestration.TaskCompleted, orchestration.TaskFailed, orchestration.TaskCanceled:
		return fmt.Errorf("assigned task %s is already terminal (%s)", task.ID, task.State)
	default:
		return fmt.Errorf("assigned task %s is not claimable in state %s", task.ID, task.State)
	}
	return nil
}

// finishAssignedTask mirrors the child Agent terminal outcome into its task.
// Cancellation releases the claim so a later scheduler run can retry it.
// Snapshot and worktree output is queued for merge first and recorded in
// result.Delivery; the task then waits for review or for the merge instead
// of completing.
func (supervisor *Supervisor) finishAssignedTask(ctx context.Context, agent orchestration.AgentSnapshot, target orchestration.AgentState, result *orchestration.AgentResult) error {
	if supervisor.tasks == nil || agent.AssignedTaskID == "" {
		if target != orchestration.AgentCompleted {
			return nil
		}
		// Unassigned work merges as soon as the merge queue reaches it.
		_, err := supervisor.deliverWork(ctx, agent, orchestration.TaskSnapshot{}, result)
		return err
	}
	task, err := supervisor.tasks.GetTask(ctx, agent.AssignedTaskID)
	if err != nil {
		return fmt.Errorf("load assigned task for completion: %w", err)
	}
	// A previous settlement may already have committed this Task transition.
	// Do not re-run delivery or bypass review when replaying its receipt.
	if target == orchestration.AgentCanceled && task.AssigneeAgentID == "" && (task.State == orchestration.TaskReady || task.State == orchestration.TaskBlocked) {
		return nil
	}
	if target == orchestration.AgentCompleted && task.AssigneeAgentID == agent.ID && (task.State == orchestration.TaskVerification || task.State == orchestration.TaskAwaitingMerge) {
		return nil
	}
	if target == orchestration.AgentCompleted && task.State == orchestration.TaskBlocked && (result.Status == orchestration.ResultPartial || result.Status == orchestration.ResultBlocked) {
		return nil
	}
	if err := checkTaskOwner(task, agent); err != nil {
		return err
	}
	if isTerminalTask(task.State) {
		return nil
	}
	correlationID := string(agent.CurrentRunID)
	switch target {
	case orchestration.AgentCompleted:
		if !isActiveTask(task.State) {
			return fmt.Errorf("cannot complete assigned task %s from state %s", task.ID, task.State)
		}
		if result.Status == orchestration.ResultPartial || result.Status == orchestration.ResultBlocked {
			reason := result.Summary
			if len(result.Blockers) > 0 {
				reason = strings.Join(result.Blockers, "; ")
			} else if len(result.Risks) > 0 {
				reason = strings.Join(result.Risks, "; ")
			}
			_, err = supervisor.tasks.TransitionTask(ctx, orchestration.TaskTransition{
				TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskBlocked,
				ActorAgentID: agent.ID, CorrelationID: correlationID, Reason: reason,
			})
			break
		}
		delivered, deliverErr := supervisor.deliverWork(ctx, agent, task, result)
		if deliverErr != nil {
			return fmt.Errorf("deliver work of task %s: %w", task.ID, deliverErr)
		}
		target := orchestration.TaskCompleted
		switch {
		case task.VerificationProfile != "" && task.State != orchestration.TaskVerification:
			target = orchestration.TaskVerification
		case delivered && task.VerificationProfile != "":
			// The task is already in review; the reviewer decides.
			return nil
		case delivered:
			target = orchestration.TaskAwaitingMerge
		}
		_, err = supervisor.tasks.TransitionTask(ctx, orchestration.TaskTransition{
			TaskID: task.ID, Expected: task.Version, Target: target,
			AssigneeAgentID: agent.ID, ActorAgentID: agent.ID, CorrelationID: correlationID, Reason: result.Summary,
		})
	case orchestration.AgentFailed:
		if !isActiveTask(task.State) {
			return fmt.Errorf("cannot fail assigned task %s from state %s", task.ID, task.State)
		}
		_, err = supervisor.tasks.TransitionTask(ctx, orchestration.TaskTransition{
			TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskFailed,
			AssigneeAgentID: agent.ID, ActorAgentID: agent.ID, CorrelationID: correlationID, Reason: result.Error,
		})
	case orchestration.AgentCanceled:
		if !isActiveTask(task.State) {
			return fmt.Errorf("cannot release assigned task %s from state %s", task.ID, task.State)
		}
		_, err = supervisor.tasks.ReleaseTask(ctx, task.ID, agent.ID, task.Version)
	}
	if err != nil {
		return fmt.Errorf("sync assigned task %s: %w", task.ID, err)
	}
	return nil
}

// releaseAssignedTask drops the claim agent holds on its unfinished task.
func (supervisor *Supervisor) releaseAssignedTask(ctx context.Context, agent orchestration.AgentSnapshot) error {
	if supervisor.tasks == nil || agent.AssignedTaskID == "" {
		return nil
	}
	task, err := supervisor.tasks.GetTask(ctx, agent.AssignedTaskID)
	if err != nil {
		return fmt.Errorf("load assigned task for release: %w", err)
	}
	if isTerminalTask(task.State) {
		return nil
	}
	if err := checkTaskOwner(task, agent); err != nil {
		return err
	}
	if !isActiveTask(task.State) {
		return fmt.Errorf("cannot release assigned task %s from state %s", task.ID, task.State)
	}
	if _, err := supervisor.tasks.ReleaseTask(ctx, task.ID, agent.ID, task.Version); err != nil {
		return fmt.Errorf("release assigned task %s: %w", task.ID, err)
	}
	return nil
}

// isTerminalTask reports whether a task is completed, failed or canceled.
func isTerminalTask(state orchestration.TaskState) bool {
	return state == orchestration.TaskCompleted || state == orchestration.TaskFailed || state == orchestration.TaskCanceled
}

// isActiveTask reports whether a task is held by its assignee: claimed, in
// progress or under verification.
func isActiveTask(state orchestration.TaskState) bool {
	return state == orchestration.TaskClaimed || state == orchestration.TaskInProgress || state == orchestration.TaskVerification
}

// checkTaskOwner rejects a task that is not assigned to agent in its Team.
func checkTaskOwner(task orchestration.TaskSnapshot, agent orchestration.AgentSnapshot) error {
	if task.TeamID != agent.TeamID || task.AssigneeAgentID != agent.ID {
		return fmt.Errorf("assigned task %s does not belong to Agent %s", task.ID, agent.ID)
	}
	return nil
}

package workspacecoord

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

const (
	// MergeApplyToolName names snapshot merge approvals, so policy and the
	// approval UI treat them like a tool call that writes the workspace.
	MergeApplyToolName = "merge_apply"

	// planRounds bounds how often a plan is rebuilt because main changed
	// between approval and the barrier.
	planRounds = 3
)

type (
	// MergeApproval is one merge waiting for a decision. A snapshot approval
	// binds to Plan.Digest, and a changed plan is asked again. A git-patch
	// approval has no plan and lists the patched paths in Paths.
	MergeApproval struct {
		Merge MergeRequest
		Plan  ApplyPlan
		Paths []string
		// Diff is a bounded unified diff of the change against main.
		Diff string
	}

	// MergeApprover decides whether a merge may change main.
	MergeApprover interface {
		// ApproveMerge requests approval for the proposed workspace merge before applying
		// it.
		ApproveMerge(context.Context, MergeApproval) (bool, error)
	}

	// MergeApproverFunc adapts a function to MergeApprover.
	MergeApproverFunc func(context.Context, MergeApproval) (bool, error)

	// MergeVerifier checks the main workspace after a merge applied. An
	// error fails the tasks the merge delivered; the change stays in main.
	MergeVerifier interface {
		VerifyMerge(context.Context, MergeRequest) error
	}
)

// ApproveMerge calls the adapted function.
func (approve MergeApproverFunc) ApproveMerge(ctx context.Context, request MergeApproval) (bool, error) {
	return approve(ctx, request)
}

// mergeReady reports whether a pending merge may start. A merge waits while
// its task is still produced or reviewed. The returned reason is set once
// the task moved on without it.
func (executor *MergeExecutor) mergeReady(ctx context.Context, merge MergeRequest) (bool, string, error) {
	if merge.TaskID == "" || executor.Tasks == nil {
		return true, "", nil
	}
	task, err := executor.Tasks.GetTask(ctx, merge.TaskID)
	if err != nil {
		return false, "", fmt.Errorf("load merge task %s: %w", merge.TaskID, err)
	}
	switch task.State {
	case orchestration.TaskAwaitingMerge:
		return true, "", nil
	case orchestration.TaskClaimed, orchestration.TaskInProgress, orchestration.TaskVerification:
		return false, "", nil
	default:
		return false, fmt.Sprintf("task %s is %s", task.ID, task.State), nil
	}
}

// runSnapshot commits a running snapshot merge. A journaled plan is resumed
// as recorded; otherwise the plan is built, approved and rebuilt under the
// barrier until its digest is stable.
func (executor *MergeExecutor) runSnapshot(ctx context.Context, running MergeRequest) (MergeRequest, error) {
	store, ok := executor.Artifacts.(ArtifactStore)
	if !ok || executor.Journal == nil || executor.snapshots() == nil {
		return executor.rejectMerge(ctx, running, "snapshot merge support is unavailable", false)
	}
	record, found, err := executor.Journal.LoadApply(ctx, running.ID)
	if err != nil {
		return running, fmt.Errorf("load apply journal of merge %s: %w", running.ID, err)
	}
	if found {
		return executor.resumeSnapshot(ctx, running, record, store)
	}
	candidate, err := LoadCandidate(ctx, store, running.PatchArtifact)
	if err != nil {
		return executor.rejectMerge(ctx, running, fmt.Sprintf("load candidate: %v", err), false)
	}
	if candidate.Digest != running.CandidateDigest {
		return executor.rejectMerge(ctx, running, "candidate digest does not match the merge request", false)
	}
	input := PlanInput{MergeID: running.ID, Candidate: candidate, WorkspaceRoot: executor.snapshots().WorkspaceRoot, Artifacts: store}
	plan, err := BuildApplyPlan(ctx, input)
	if err != nil {
		return running, fmt.Errorf("build apply plan of merge %s: %w", running.ID, err)
	}
	for round := 1; ; round++ {
		if len(plan.Conflicts) > 0 {
			return executor.markConflict(ctx, running, conflictReason(plan))
		}
		approved, err := executor.approve(ctx, running, plan, store)
		if err != nil {
			return running, err
		}
		if !approved {
			return executor.rejectMerge(ctx, running, "apply plan was denied", true)
		}
		committed, current, err := executor.commitIfStable(ctx, running, input, plan, store)
		if err != nil {
			if errors.Is(err, ErrApplyDiverged) {
				return executor.markConflict(ctx, running, err.Error())
			}
			return running, err
		}
		if committed {
			return executor.markApplied(ctx, running, plan.Digest)
		}
		if round == planRounds {
			// Main keeps changing; the merge stays running for the next pass.
			return running, nil
		}
		plan = current
	}
}

// commitIfStable rebuilds the plan under the barrier and commits it when the
// digest still matches the approved plan. Otherwise it returns the new plan.
func (executor *MergeExecutor) commitIfStable(ctx context.Context, running MergeRequest, input PlanInput, approved ApplyPlan, store ArtifactStore) (bool, ApplyPlan, error) {
	release, err := executor.hold(ctx, running)
	if err != nil {
		return false, ApplyPlan{}, err
	}
	defer release()
	current, err := BuildApplyPlan(ctx, input)
	if err != nil {
		return false, ApplyPlan{}, fmt.Errorf("rebuild apply plan of merge %s: %w", running.ID, err)
	}
	if current.Digest != approved.Digest {
		return false, current, nil
	}
	if err := VerifyPreimages(input.WorkspaceRoot, approved); err != nil {
		return false, ApplyPlan{}, err
	}
	if err := executor.beginMutation(ctx, running.ID); err != nil {
		return false, ApplyPlan{}, err
	}
	if err := CommitPlan(ctx, input.WorkspaceRoot, approved, store, executor.Journal, nil); err != nil {
		return false, ApplyPlan{}, err
	}
	running.PlanDigest = approved.Digest
	if err := executor.prepareFinalization(ctx, running); err != nil {
		return false, ApplyPlan{}, err
	}
	return true, current, nil
}

// resumeSnapshot finishes a journaled commit after a restart. The approval
// of the recorded plan still holds because its digest is unchanged.
func (executor *MergeExecutor) resumeSnapshot(ctx context.Context, running MergeRequest, record ApplyRecord, store ArtifactStore) (MergeRequest, error) {
	release, err := executor.hold(ctx, running)
	if err != nil {
		return running, err
	}
	if err = executor.beginMutation(ctx, running.ID); err == nil {
		err = CommitPlan(ctx, executor.snapshots().WorkspaceRoot, record.Plan, store, executor.Journal, nil)
	}
	if err == nil {
		for _, entry := range record.Plan.Entries {
			if err = matchesImage(executor.snapshots().WorkspaceRoot, entry.Path, entry.Postimage); err != nil {
				break
			}
		}
	}
	if err == nil {
		running.PlanDigest = record.Plan.Digest
		err = executor.prepareFinalization(ctx, running)
	}
	release()
	if errors.Is(err, ErrApplyDiverged) {
		return executor.markConflict(ctx, running, "resume journaled commit: "+err.Error())
	}
	if err != nil {
		return running, fmt.Errorf("resume merge %s: %w", running.ID, err)
	}
	return executor.markApplied(ctx, running, record.Plan.Digest)
}

// approve asks for a plan decision unless auto merge is on. A plan without
// entries changes nothing in main and needs no approval.
func (executor *MergeExecutor) approve(ctx context.Context, running MergeRequest, plan ApplyPlan, artifacts ArtifactReader) (bool, error) {
	if executor.AutoMerge || len(plan.Entries) == 0 {
		return true, nil
	}
	if executor.Approver == nil {
		return false, nil
	}
	diff, err := PlanDiff(ctx, executor.snapshots().WorkspaceRoot, plan, artifacts)
	if err != nil {
		return false, fmt.Errorf("describe apply plan of merge %s: %w", running.ID, err)
	}
	approved, err := executor.Approver.ApproveMerge(ctx, MergeApproval{Merge: running, Plan: plan, Diff: diff})
	if err != nil {
		return false, fmt.Errorf("approve merge %s: %w", running.ID, err)
	}
	return approved, nil
}

// hold pauses lease-honoring writers of main for the merge's Agent.
func (executor *MergeExecutor) hold(ctx context.Context, merge MergeRequest) (func(), error) {
	// Independent passes may wait for different approvals concurrently, but
	// commits and checks still share one cancellable workspace execution gate.
	executor.mutex.Lock()
	if executor.workspaceGate == nil {
		executor.workspaceGate = make(chan struct{}, 1)
	}
	gate := executor.workspaceGate
	executor.mutex.Unlock()
	select {
	case gate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	unlock := func() { <-gate }
	snapshots := executor.snapshots()
	if snapshots == nil || snapshots.Barrier == nil {
		return unlock, nil
	}
	release, err := snapshots.Barrier.Hold(ctx, merge.TeamID, merge.AgentID, "")
	if err != nil {
		unlock()
		return nil, fmt.Errorf("hold workspace barrier for merge %s: %w", merge.ID, err)
	}
	return func() { release(); unlock() }, nil
}

// markApplied records the commit, settles the delivered tasks after the
// post-merge check and removes the snapshots whose work reached main.
func (executor *MergeExecutor) markApplied(ctx context.Context, running MergeRequest, planDigest string) (MergeRequest, error) {
	applied, err := executor.Repository.TransitionMerge(ctx, MergeTransition{
		MergeID: running.ID, Expected: running.Version, Target: MergeApplied, PlanDigest: planDigest,
		ActorAgentID: running.AgentID, CorrelationID: running.CorrelationID, Reason: "snapshot plan applied",
	})
	if err != nil {
		return running, fmt.Errorf("complete merge %s: %w", running.ID, err)
	}
	return applied, executor.finalizeApplied(ctx, applied)
}

// rejectMerge closes a merge that will not be applied. A denied change
// also fails its task, since the work was refused rather than outdated.
// The snapshot or worktree stays on disk for review.
func (executor *MergeExecutor) rejectMerge(ctx context.Context, merge MergeRequest, reason string, failTask bool) (MergeRequest, error) {
	rejected, err := executor.Repository.TransitionMerge(ctx, MergeTransition{
		MergeID: merge.ID, Expected: merge.Version, Target: MergeRejected,
		ActorAgentID: merge.AgentID, CorrelationID: merge.CorrelationID, Reason: reason,
	})
	if errors.Is(err, orchestration.ErrSequenceConflict) {
		return merge, nil
	}
	if err != nil {
		return merge, fmt.Errorf("reject merge %s: %w", merge.ID, err)
	}
	var errs []error
	if failTask && merge.TaskID != "" && executor.Tasks != nil {
		errs = append(errs, executor.transitionTask(ctx, merge.TaskID, orchestration.TaskFailed, merge.AgentID, reason))
	}
	if executor.Coordinator != nil {
		errs = append(errs, executor.Coordinator.FinalizeView(ctx, merge.ViewID, "merge rejected: "+reason))
	}
	return rejected, errors.Join(errs...)
}

// Discard rejects an open merge at the user's request. It fails the task
// like a denied change and keeps the candidate or worktree on disk for
// review. Approval waits can be canceled; a begun commit cannot be discarded.
func (executor *MergeExecutor) Discard(ctx context.Context, mergeID orchestration.MergeID, reason string) (MergeRequest, error) {
	if executor == nil || executor.Repository == nil {
		return MergeRequest{}, errors.New("merge executor is unavailable")
	}
	executor.mutex.Lock()
	defer executor.mutex.Unlock()
	if active := executor.active[mergeID]; active != nil {
		if active.mutating {
			return MergeRequest{}, errors.New("merge commit has started and cannot be discarded")
		}
		active.cancel()
	}
	merge, err := executor.Repository.GetMerge(ctx, mergeID)
	if err != nil {
		return MergeRequest{}, fmt.Errorf("load merge %s: %w", mergeID, err)
	}
	if merge.State != MergePending && merge.State != MergeConflicted && merge.State != MergeRunning {
		return merge, fmt.Errorf("merge %s is %s and cannot be discarded", merge.ID, merge.State)
	}
	if executor.Journal != nil {
		if _, found, err := executor.Journal.LoadApply(ctx, merge.ID); err != nil {
			return merge, err
		} else if found {
			return merge, errors.New("journaled merge cannot be discarded")
		}
	}
	if store := executor.finalizations(); store != nil {
		if _, found, err := store.LoadMergeFinalization(ctx, merge.ID); err != nil {
			return merge, err
		} else if found {
			return merge, errors.New("committed merge cannot be discarded")
		}
	}
	if strings.TrimSpace(reason) == "" {
		reason = "candidate discarded by the user"
	}
	return executor.rejectMerge(ctx, merge, reason, true)
}

// closeUnusedResolution closes a conflicted snapshot merge whose resolution
// task completed without a superseding candidate. The resolution kept main
// as it is, so the original task is complete.
func (executor *MergeExecutor) closeUnusedResolution(ctx context.Context, merge MergeRequest) error {
	task, err := executor.Tasks.GetTask(ctx, merge.ResolutionTaskID)
	if err != nil {
		return fmt.Errorf("load merge resolution task %s: %w", merge.ResolutionTaskID, err)
	}
	if task.State != orchestration.TaskCompleted {
		return nil
	}
	reason := "resolution task " + string(task.ID) + " kept the main workspace"
	if _, err := executor.Repository.TransitionMerge(ctx, MergeTransition{
		MergeID: merge.ID, Expected: merge.Version, Target: MergeRejected,
		ActorAgentID: task.AssigneeAgentID, CorrelationID: merge.CorrelationID, Reason: reason,
	}); err != nil {
		if errors.Is(err, orchestration.ErrSequenceConflict) {
			return nil
		}
		return fmt.Errorf("close resolved merge %s: %w", merge.ID, err)
	}
	errs := []error{executor.finishAwaitingTask(ctx, merge.TaskID, orchestration.TaskCompleted, task.AssigneeAgentID, reason)}
	if executor.Coordinator != nil {
		errs = append(errs, executor.Coordinator.FinalizeView(ctx, merge.ViewID, reason))
	}
	return errors.Join(errs...)
}

// finishAwaitingTask moves a task that waits for its merge to target. Tasks
// in other states were already finished or moved on and are left alone.
func (executor *MergeExecutor) finishAwaitingTask(ctx context.Context, taskID orchestration.TaskID, target orchestration.TaskState, actor orchestration.AgentID, reason string) error {
	if taskID == "" || executor.Tasks == nil {
		return nil
	}
	task, err := executor.Tasks.GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("load delivered task %s: %w", taskID, err)
	}
	if task.State != orchestration.TaskAwaitingMerge {
		return nil
	}
	return executor.transitionTask(ctx, taskID, target, actor, reason)
}

// transitionTask moves a task at its current version.
func (executor *MergeExecutor) transitionTask(ctx context.Context, taskID orchestration.TaskID, target orchestration.TaskState, actor orchestration.AgentID, reason string) error {
	task, err := executor.Tasks.GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("load task %s: %w", taskID, err)
	}
	if _, err := executor.Tasks.TransitionTask(ctx, orchestration.TaskTransition{
		TaskID: task.ID, Expected: task.Version, Target: target, ActorAgentID: actor, Reason: reason,
	}); err != nil {
		return fmt.Errorf("move task %s to %s: %w", task.ID, target, err)
	}
	return nil
}

// snapshots returns the snapshot store of the coordinator, if any.
func (executor *MergeExecutor) snapshots() *SnapshotStore {
	if executor.Coordinator == nil {
		return nil
	}
	return executor.Coordinator.Snapshots
}

// conflictReason lists the conflicted paths of a plan for the resolution
// task.
func conflictReason(plan ApplyPlan) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "%d path(s) conflict with the main workspace:", len(plan.Conflicts))
	for _, conflict := range plan.Conflicts {
		fmt.Fprintf(&builder, "\n- %s: %s", conflict.Path, conflict.Reason)
	}
	return builder.String()
}

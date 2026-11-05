package workspacecoord

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

// ArtifactReader resolves the durable patch reference stored on a merge
// request, typically from a content-addressed artifact store.
type ArtifactReader interface {
	ReadArtifact(context.Context, string) ([]byte, error)
}

// ArtifactReaderFunc adapts a function to ArtifactReader.
type ArtifactReaderFunc func(context.Context, string) ([]byte, error)

func (reader ArtifactReaderFunc) ReadArtifact(ctx context.Context, reference string) ([]byte, error) {
	if reader == nil {
		return nil, errors.New("nil artifact reader")
	}
	return reader(ctx, reference)
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

// MergeExecutor serializes merge application per process and keeps every
// state transition durable. A second process can safely race because the
// optimistic merge version rejects stale transitions.
type MergeExecutor struct {
	Repository  Repository
	Coordinator *Coordinator
	// Worktrees applies git-patch merges. Snapshot merges do not need it.
	Worktrees *WorktreeManager
	Artifacts ArtifactReader
	Tasks     orchestration.TaskRepository
	// Journal records snapshot commits. It defaults to Repository when the
	// repository implements ApplyJournal.
	Journal ApplyJournal
	// Approver accepts snapshot apply plans. AutoMerge applies conflict-free
	// plans without asking; without either, snapshot merges stay queued for
	// an executor that can decide.
	Approver  MergeApprover
	AutoMerge bool
	// Verifier checks main after each applied merge before the waiting
	// tasks complete. Without one, the review of the candidate is the gate.
	Verifier MergeVerifier

	mutex         sync.Mutex // protects active claims, never held while asking a human
	active        map[orchestration.MergeID]*mergeExecution
	workspaceGate chan struct{} // serializes commit/check scopes, not approvals
}

type mergeExecution struct {
	cancel   context.CancelFunc
	mutating bool
}

func NewMergeExecutor(repository Repository, coordinator *Coordinator, worktrees *WorktreeManager, artifacts ArtifactReader, tasks orchestration.TaskRepository) (*MergeExecutor, error) {
	if repository == nil {
		return nil, errors.New("merge executor requires a repository")
	}
	if worktrees == nil && coordinator != nil {
		worktrees = coordinator.Worktrees
	}
	if worktrees == nil && (coordinator == nil || coordinator.Snapshots == nil) {
		return nil, errors.New("merge executor requires a worktree manager or a snapshot store")
	}
	executor := &MergeExecutor{Repository: repository, Coordinator: coordinator, Worktrees: worktrees, Artifacts: artifacts, Tasks: tasks}
	if journal, ok := repository.(ApplyJournal); ok {
		executor.Journal = journal
	}
	return executor, nil
}

// RunPending applies queued merges with an explicit approver or host AutoMerge
// grant. Calling this method is not itself evidence of approval.
func (executor *MergeExecutor) RunPending(ctx context.Context, teamID orchestration.TeamID, limit int) ([]MergeRequest, error) {
	return executor.runPending(ctx, teamID, limit)
}

// RunPendingAutomatic runs merges for a background pass. Every merge waits
// for its task and asks the approver unless auto merge is on; without
// either, merges stay queued for an executor that can decide.
func (executor *MergeExecutor) RunPendingAutomatic(ctx context.Context, teamID orchestration.TeamID, limit int) ([]MergeRequest, error) {
	return executor.runPending(ctx, teamID, limit)
}

// runPending reconciles committed deliveries before admitting new merges.
func (executor *MergeExecutor) runPending(ctx context.Context, teamID orchestration.TeamID, limit int) ([]MergeRequest, error) {
	if executor == nil || executor.Repository == nil {
		return nil, errors.New("merge executor is unavailable")
	}
	if err := teamID.Validate(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 100
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if store := executor.finalizations(); store != nil {
		unfinished, err := store.PendingMergeFinalizations(ctx, teamID, limit)
		if err != nil {
			return nil, fmt.Errorf("list unfinished merge deliveries: %w", err)
		}
		for _, merge := range unfinished {
			runCtx, release, claimed := executor.claim(ctx, merge.ID)
			if !claimed {
				continue
			}
			err := executor.finalizeApplied(runCtx, merge)
			release()
			if err != nil {
				return nil, fmt.Errorf("reconcile applied merge %s: %w", merge.ID, err)
			}
		}
	}
	if err := executor.requeueCompletedResolutions(ctx, teamID); err != nil {
		return nil, err
	}
	pending, err := executor.Repository.ListMerges(ctx, MergeQuery{TeamID: teamID, States: []MergeState{MergePending, MergeRunning}})
	if err != nil {
		return nil, fmt.Errorf("list pending merges: %w", err)
	}
	if len(pending) > limit {
		pending = pending[:limit]
	}
	results := make([]MergeRequest, 0, len(pending))
	for _, merge := range pending {
		runCtx, release, claimed := executor.claim(ctx, merge.ID)
		if !claimed {
			continue
		}
		updated, runErr := executor.runOne(runCtx, merge)
		release()
		if runErr != nil {
			return results, runErr
		}
		results = append(results, updated)
	}
	return results, nil
}

// requeueCompletedResolutions settles conflicted merges whose resolution
// task completed.
func (executor *MergeExecutor) requeueCompletedResolutions(ctx context.Context, teamID orchestration.TeamID) error {
	if executor.Tasks == nil {
		return nil
	}
	conflicted, err := executor.Repository.ListMerges(ctx, MergeQuery{TeamID: teamID, States: []MergeState{MergeConflicted}})
	if err != nil {
		return fmt.Errorf("list conflicted merges: %w", err)
	}
	for _, merge := range conflicted {
		var err error
		if merge, err = executor.ensureResolutionTask(ctx, merge); err != nil {
			return err
		}
		// A snapshot resolution delivers a superseding merge instead.
		if merge.Kind == MergeSnapshot {
			if err := executor.closeUnusedResolution(ctx, merge); err != nil {
				return err
			}
			continue
		}
		task, taskErr := executor.Tasks.GetTask(ctx, merge.ResolutionTaskID)
		if taskErr != nil {
			return fmt.Errorf("load merge resolution task %s: %w", merge.ResolutionTaskID, taskErr)
		}
		if task.State != orchestration.TaskCompleted {
			continue
		}
		if _, transitionErr := executor.Repository.TransitionMerge(ctx, MergeTransition{
			MergeID: merge.ID, Expected: merge.Version, Target: MergePending,
			ResolutionTaskID: merge.ResolutionTaskID,
			ActorAgentID:     task.AssigneeAgentID, CorrelationID: merge.CorrelationID,
			Reason: "merge resolution task completed",
		}); transitionErr != nil && !errors.Is(transitionErr, orchestration.ErrSequenceConflict) {
			return fmt.Errorf("requeue resolved merge %s: %w", merge.ID, transitionErr)
		}
	}
	return nil
}

// runOne settles one merge after the task and host approval gates pass.
func (executor *MergeExecutor) runOne(ctx context.Context, pending MergeRequest) (MergeRequest, error) {
	if !executor.decidesMerges() {
		return pending, nil
	}
	if pending.State == MergePending {
		ready, reject, err := executor.mergeReady(ctx, pending)
		if err != nil {
			return pending, err
		}
		if reject != "" && pending.Kind == MergeSnapshot {
			return executor.rejectMerge(ctx, pending, reject, false)
		}
		if !ready {
			// A git-patch merge whose task moved on stays for merge_run.
			return pending, nil
		}
	}
	running := pending
	if pending.State == MergePending {
		var err error
		running, err = executor.Repository.TransitionMerge(ctx, MergeTransition{
			MergeID: pending.ID, Expected: pending.Version, Target: MergeRunning,
			ActorAgentID: pending.AgentID, CorrelationID: pending.CorrelationID,
		})
		if err != nil {
			if errors.Is(err, orchestration.ErrSequenceConflict) || errors.Is(err, orchestration.ErrInvalidEvent) {
				return pending, nil
			}
			return pending, fmt.Errorf("start merge %s: %w", pending.ID, err)
		}
	} else if pending.State != MergeRunning {
		return pending, nil
	}
	if err := contextError(ctx); err != nil {
		return running, err
	}
	if running.Kind == MergeSnapshot {
		return executor.runSnapshot(ctx, running)
	}
	return executor.runPatch(ctx, running)
}

// runPatch applies a running git-patch merge to the main workspace after
// requesting approval of its patch. The patch is applied under the
// workspace barrier, and the task waiting for it is settled by the
// post-merge check.
func (executor *MergeExecutor) runPatch(ctx context.Context, running MergeRequest) (MergeRequest, error) {
	if executor.Worktrees == nil {
		return executor.markConflict(ctx, running, "git worktree support is unavailable")
	}
	base, err := executor.Worktrees.ResolveHead(ctx)
	if err != nil {
		return executor.markConflict(ctx, running, fmt.Sprintf("resolve workspace head: %v", err))
	}
	if base != running.BaseRevision {
		return executor.markConflict(ctx, running, fmt.Sprintf("workspace head %s differs from merge base %s", base, running.BaseRevision))
	}
	if executor.Artifacts == nil {
		return executor.markConflict(ctx, running, "patch artifact reader is unavailable")
	}
	patch, err := executor.Artifacts.ReadArtifact(ctx, running.PatchArtifact)
	if err != nil {
		return executor.markConflict(ctx, running, fmt.Sprintf("read patch artifact: %v", err))
	}
	if len(patch) > 0 {
		approved, err := executor.approvePatch(ctx, running, patch)
		if err != nil {
			return running, err
		}
		if !approved {
			return executor.rejectMerge(ctx, running, "patch was denied", true)
		}
		release, err := executor.hold(ctx, running)
		if err != nil {
			return running, err
		}
		if err = executor.beginMutation(ctx, running.ID); err == nil {
			err = executor.Worktrees.ApplyPatch(ctx, patch)
		}
		if err == nil {
			err = executor.prepareFinalization(ctx, running)
		}
		release()
		if err != nil {
			return executor.markConflict(ctx, running, fmt.Sprintf("apply patch: %v", err))
		}
	}
	if len(patch) == 0 {
		release, err := executor.hold(ctx, running)
		if err != nil {
			return running, err
		}
		err = executor.prepareFinalization(ctx, running)
		release()
		if err != nil {
			return running, err
		}
	}
	applied, err := executor.Repository.TransitionMerge(ctx, MergeTransition{
		MergeID: running.ID, Expected: running.Version, Target: MergeApplied,
		ActorAgentID: running.AgentID, CorrelationID: running.CorrelationID, Reason: "patch applied",
	})
	if err != nil {
		return running, fmt.Errorf("complete merge %s: %w", running.ID, err)
	}
	return applied, executor.finalizeApplied(ctx, applied)
}

// approvePatch asks for a decision on a git-patch merge unless auto merge
// is on. The request shows the bounded patch and the paths it writes.
func (executor *MergeExecutor) approvePatch(ctx context.Context, running MergeRequest, patch []byte) (bool, error) {
	if executor.AutoMerge {
		return true, nil
	}
	if executor.Approver == nil {
		return false, nil
	}
	diff := PatchDiff(patch)
	approved, err := executor.Approver.ApproveMerge(ctx, MergeApproval{Merge: running, Paths: PatchPaths(patch), Diff: diff})
	if err != nil {
		return false, fmt.Errorf("approve merge %s: %w", running.ID, err)
	}
	return approved, nil
}

// decidesMerges reports whether the executor may settle gated merges. One
// without an approver leaves them durable for a process that can ask.
func (executor *MergeExecutor) decidesMerges() bool {
	return executor.AutoMerge || executor.Approver != nil
}

// RequeueResolved moves a conflicted merge back to the durable queue after a
// resolution task has completed. The task identity prevents an unrelated
// completed task from accidentally unlocking a merge.
func (executor *MergeExecutor) RequeueResolved(ctx context.Context, mergeID orchestration.MergeID, resolutionTaskID orchestration.TaskID) (MergeRequest, error) {
	if executor == nil || executor.Repository == nil || executor.Tasks == nil {
		return MergeRequest{}, errors.New("merge resolution is unavailable")
	}
	if err := mergeID.Validate(); err != nil {
		return MergeRequest{}, err
	}
	if err := resolutionTaskID.Validate(); err != nil {
		return MergeRequest{}, err
	}
	merge, err := executor.Repository.GetMerge(ctx, mergeID)
	if err != nil {
		return MergeRequest{}, err
	}
	if merge.State != MergeConflicted {
		return MergeRequest{}, fmt.Errorf("%w: merge %s is %s", orchestration.ErrInvalidEvent, mergeID, merge.State)
	}
	task, err := executor.Tasks.GetTask(ctx, resolutionTaskID)
	if err != nil {
		return MergeRequest{}, err
	}
	if task.TeamID != merge.TeamID || task.State != orchestration.TaskCompleted {
		return MergeRequest{}, fmt.Errorf("%w: resolution task is not completed for merge", orchestration.ErrInvalidEvent)
	}
	if merge.ResolutionTaskID != "" && merge.ResolutionTaskID != resolutionTaskID {
		return MergeRequest{}, fmt.Errorf("%w: task is not linked to merge", orchestration.ErrInvalidEvent)
	}
	if merge.ResolutionTaskID == "" && merge.TaskID == "" {
		return MergeRequest{}, fmt.Errorf("%w: merge has no resolution task association", orchestration.ErrInvalidEvent)
	}
	if merge.TaskID != "" && task.ParentTaskID != merge.TaskID {
		return MergeRequest{}, fmt.Errorf("%w: resolution task does not belong to merge task", orchestration.ErrInvalidEvent)
	}
	return executor.Repository.TransitionMerge(ctx, MergeTransition{
		MergeID: merge.ID, Expected: merge.Version, Target: MergePending,
		ResolutionTaskID: resolutionTaskID,
		ActorAgentID:     task.AssigneeAgentID, CorrelationID: merge.CorrelationID,
		Reason: "merge conflict resolved",
	})
}

func (executor *MergeExecutor) markConflict(ctx context.Context, running MergeRequest, reason string) (MergeRequest, error) {
	conflicted, err := executor.Repository.TransitionMerge(ctx, MergeTransition{
		MergeID: running.ID, Expected: running.Version, Target: MergeConflicted,
		ActorAgentID: running.AgentID, CorrelationID: running.CorrelationID, Reason: reason,
	})
	if err != nil {
		return running, fmt.Errorf("record merge conflict %s: %w", running.ID, err)
	}
	return executor.ensureResolutionTask(ctx, conflicted)
}

// ensureResolutionTask repairs both crash windows after the conflicted queue
// transition: before task creation and between task creation and linking. The
// stable request key makes creating the same resolution safe to replay.
func (executor *MergeExecutor) ensureResolutionTask(ctx context.Context, conflicted MergeRequest) (MergeRequest, error) {
	if executor.Tasks != nil {
		var resolutionTask orchestration.TaskSnapshot
		var taskErr error
		if conflicted.ResolutionTaskID != "" {
			resolutionTask, taskErr = executor.Tasks.GetTask(ctx, conflicted.ResolutionTaskID)
		} else {
			resolutionTask, taskErr = executor.Tasks.CreateTask(ctx, orchestration.TaskSpec{
				TeamID: conflicted.TeamID, ActorAgentID: conflicted.AgentID, ParentTaskID: conflicted.TaskID,
				Title:           "Resolve merge conflict " + string(conflicted.ID),
				Description:     "Resolve the workspace merge conflict and re-run verification.\n\n" + conflicted.Reason,
				RequiredProfile: "worker", IdempotencyKey: "merge-resolution:" + string(conflicted.ID), CorrelationID: string(conflicted.ID),
			})
			if taskErr != nil {
				return conflicted, fmt.Errorf("create merge resolution task: %w", taskErr)
			}
			if linker, ok := executor.Repository.(ResolutionLinker); ok {
				linked, linkErr := linker.LinkMergeResolutionTask(ctx, conflicted.ID, resolutionTask.ID)
				if linkErr != nil {
					return conflicted, fmt.Errorf("link merge resolution task: %w", linkErr)
				}
				conflicted = linked
			}
		}
		if taskErr != nil {
			return conflicted, fmt.Errorf("load merge resolution task: %w", taskErr)
		}
		// A snapshot conflict is resolved in a snapshot seeded from the
		// candidate, so its task is ready for dispatch at once. A git-patch
		// resolution task stays a draft for its parent to plan.
		if conflicted.Kind == MergeSnapshot && resolutionTask.State == orchestration.TaskDraft {
			if _, err := executor.Tasks.TransitionTask(ctx, orchestration.TaskTransition{
				TaskID: resolutionTask.ID, Expected: resolutionTask.Version, Target: orchestration.TaskReady,
				ActorAgentID: conflicted.AgentID, CorrelationID: string(conflicted.ID), Reason: "merge conflict needs resolution",
			}); err != nil {
				return conflicted, fmt.Errorf("ready merge resolution task: %w", err)
			}
		}
	}
	return conflicted, nil
}

func (executor *MergeExecutor) claim(ctx context.Context, id orchestration.MergeID) (context.Context, func(), bool) {
	executor.mutex.Lock()
	defer executor.mutex.Unlock()
	if executor.active == nil {
		executor.active = make(map[orchestration.MergeID]*mergeExecution)
	}
	if executor.active[id] != nil {
		return nil, nil, false
	}
	runCtx, cancel := context.WithCancel(ctx)
	executor.active[id] = &mergeExecution{cancel: cancel}
	return runCtx, func() {
		cancel()
		executor.mutex.Lock()
		delete(executor.active, id)
		executor.mutex.Unlock()
	}, true
}

func (executor *MergeExecutor) beginMutation(ctx context.Context, id orchestration.MergeID) error {
	executor.mutex.Lock()
	defer executor.mutex.Unlock()
	if err := contextError(ctx); err != nil {
		return err
	}
	if active := executor.active[id]; active != nil {
		active.mutating = true
	}
	return nil
}

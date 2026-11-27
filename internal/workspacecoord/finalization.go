package workspacecoord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

// FinalizationState separates committing bytes from checking and delivering
// them. Dispatched is durable before invoking an external verification command:
// recovery must not execute that command a second time without its outcome.
type FinalizationState string

const (
	finalizationWriteTimeout = 5 * time.Second

	FinalizationPrepared   FinalizationState = "prepared"
	FinalizationDispatched FinalizationState = "dispatched"
	FinalizationCompleted  FinalizationState = "completed"
	FinalizationUnknown    FinalizationState = "unknown"
)

// MergeFinalization binds the post-merge check to an immutable merge and a
// content identity of main. Settled is recorded only after task and view
// reconciliation succeeded; those operations can be retried without rerunning
// the verifier.
type MergeFinalization struct {
	MergeID            orchestration.MergeID
	BindingDigest      string
	WorkspaceDigest    string
	VerificationDigest string
	State              FinalizationState
	TargetState        orchestration.TaskState
	Reason             string
	Settled            bool
}

// VerifierIdentity binds prepared verification to the host-configured command,
// timeout and execution protocol. Production verifiers must implement it;
// injected test verifiers without configuration use their Go type identity.
type VerifierIdentity interface {
	VerificationIdentity() string
}

func (executor *MergeExecutor) verificationIdentity() string {
	if executor.Verifier == nil {
		return "gsea.merge-verification.none.v1"
	}
	if identity, ok := executor.Verifier.(VerifierIdentity); ok {
		return identity.VerificationIdentity()
	}
	return "gsea.merge-verification.injected.v1:" + reflect.TypeOf(executor.Verifier).String()
}

// FinalizationStore owns durable verification receipts and the bounded recovery
// query. It is optional for test repositories without external verification.
type FinalizationStore interface {
	// BeginMergeFinalization persists the receipt identifying the merge and its pending
	// finalization work.
	BeginMergeFinalization(context.Context, MergeFinalization) (MergeFinalization, error)
	// LoadMergeFinalization looks up the durable receipt for a merge and reports whether
	// one exists.
	LoadMergeFinalization(context.Context, orchestration.MergeID) (MergeFinalization, bool, error)
	// TransitionMergeFinalization advances the receipt from its expected previous state,
	// checking that the merge binding matches.
	TransitionMergeFinalization(context.Context, MergeFinalization, FinalizationState) (MergeFinalization, error)
	SettleMergeFinalization(context.Context, orchestration.MergeID, string) error
	PendingMergeFinalizations(context.Context, orchestration.TeamID, int) ([]MergeRequest, error)
}

// MergeBindingDigest deliberately excludes mutable queue state and versions.
// A receipt cannot be reused for another candidate, patch or committed plan.
func MergeBindingDigest(merge MergeRequest) string {
	encoded, _ := json.Marshal(struct {
		ID        orchestration.MergeID
		Kind      MergeKind
		View      orchestration.WorkspaceViewID
		Base      string
		Candidate string
		Patch     string
		Plan      string
	}{merge.ID, merge.Kind, merge.ViewID, merge.BaseRevision, merge.CandidateDigest, merge.PatchArtifact, merge.PlanDigest})
	return "sha256:" + hashBytes(encoded)
}

func (executor *MergeExecutor) finalizations() FinalizationStore {
	store, _ := executor.Repository.(FinalizationStore)
	return store
}

func (executor *MergeExecutor) workspaceRoot() string {
	if snapshots := executor.snapshots(); snapshots != nil {
		return snapshots.WorkspaceRoot
	}
	if executor.Worktrees != nil {
		return executor.Worktrees.Root
	}
	return ""
}

// prepareFinalization is called under the workspace barrier immediately after
// the commit. A restarted, finished snapshot journal can also rebuild this
// proof, but only after its exact postimages have been checked.
func (executor *MergeExecutor) prepareFinalization(ctx context.Context, merge MergeRequest) error {
	store := executor.finalizations()
	if store == nil {
		if executor.Verifier != nil {
			return errors.New("post-merge verification requires a durable finalization store")
		}
		return nil
	}
	if receipt, found, err := store.LoadMergeFinalization(ctx, merge.ID); err != nil {
		return err
	} else if found {
		if receipt.BindingDigest != MergeBindingDigest(merge) {
			return errors.New("merge finalization binding differs from applied content")
		}
		return nil
	}
	manifest, err := BuildManifest(ctx, executor.workspaceRoot(), nil)
	if err != nil {
		return fmt.Errorf("identify merged workspace: %w", err)
	}
	_, err = store.BeginMergeFinalization(ctx, MergeFinalization{
		MergeID: merge.ID, BindingDigest: MergeBindingDigest(merge), WorkspaceDigest: manifest.Digest(),
		VerificationDigest: executor.verificationIdentity(),
		State:              FinalizationPrepared,
	})
	return err
}

// finalizeApplied retries only durable post-commit reconciliation. It never
// applies bytes or asks for approval again.
func (executor *MergeExecutor) finalizeApplied(ctx context.Context, applied MergeRequest) error {
	delivered := []MergeRequest{applied}
	seen := map[orchestration.MergeID]bool{applied.ID: true}
	for previous := applied.Supersedes; previous != ""; {
		if seen[previous] {
			return errors.New("cyclic superseded merge chain")
		}
		seen[previous] = true
		merge, err := executor.Repository.GetMerge(ctx, previous)
		if err != nil {
			return fmt.Errorf("load superseded merge %s: %w", previous, err)
		}
		delivered = append(delivered, merge)
		previous = merge.Supersedes
	}
	store := executor.finalizations()
	if store == nil {
		if executor.Verifier != nil {
			return errors.New("post-merge verification requires a durable finalization store")
		}
		return executor.settleDelivered(ctx, applied, delivered, orchestration.TaskCompleted, "merge "+string(applied.ID)+" applied")
	}
	receipt, found, err := store.LoadMergeFinalization(ctx, applied.ID)
	if err != nil {
		return err
	}
	if found && receipt.BindingDigest != MergeBindingDigest(applied) {
		return errors.New("merge finalization binding differs from applied merge")
	}
	if found && receipt.Settled {
		return nil
	}
	if !found {
		// Legacy applied merges have no proof of which workspace was checked.
		// Without a verifier their candidate review remains the gate. With a
		// verifier, fail closed rather than checking unrelated current bytes.
		receipt = MergeFinalization{MergeID: applied.ID, BindingDigest: MergeBindingDigest(applied), State: FinalizationPrepared}
		if receipt, err = store.BeginMergeFinalization(ctx, receipt); err != nil {
			return err
		}
	}
	if receipt.State == FinalizationPrepared || receipt.State == FinalizationDispatched {
		receipt, err = executor.finishVerification(ctx, applied, receipt, store)
		if err != nil {
			return err
		}
	}
	if receipt.State != FinalizationCompleted && receipt.State != FinalizationUnknown {
		return fmt.Errorf("unknown merge finalization state %q", receipt.State)
	}
	if err := executor.settleDelivered(ctx, applied, delivered, receipt.TargetState, receipt.Reason); err != nil {
		return err
	}
	return store.SettleMergeFinalization(ctx, applied.ID, receipt.BindingDigest)
}

func (executor *MergeExecutor) finishVerification(ctx context.Context, applied MergeRequest, receipt MergeFinalization, store FinalizationStore) (MergeFinalization, error) {
	previous := receipt.State
	receipt.TargetState = orchestration.TaskCompleted
	receipt.Reason = "merge " + string(applied.ID) + " applied"
	needsCheck := executor.Verifier != nil
	if previous == FinalizationDispatched {
		receipt.State, receipt.TargetState = FinalizationUnknown, orchestration.TaskFailed
		receipt.Reason += "; post-merge verification outcome is unknown after interruption; not rerun"
		return store.TransitionMergeFinalization(ctx, receipt, previous)
	}
	if receipt.VerificationDigest != "" && receipt.VerificationDigest != executor.verificationIdentity() {
		receipt.State, receipt.TargetState = FinalizationCompleted, orchestration.TaskFailed
		receipt.Reason += "; post-merge verification configuration changed before dispatch"
		return store.TransitionMergeFinalization(ctx, receipt, previous)
	}
	if receipt.VerificationDigest == "" && needsCheck {
		receipt.State, receipt.TargetState = FinalizationCompleted, orchestration.TaskFailed
		receipt.Reason += "; original verification configuration is unavailable"
		return store.TransitionMergeFinalization(ctx, receipt, previous)
	}
	if !needsCheck {
		receipt.State = FinalizationCompleted
		return store.TransitionMergeFinalization(ctx, receipt, previous)
	}
	release, err := executor.hold(ctx, applied)
	if err != nil {
		return receipt, err
	}
	defer release()
	manifest, err := BuildManifest(ctx, executor.workspaceRoot(), nil)
	if err != nil {
		return receipt, err
	}
	if receipt.WorkspaceDigest == "" || manifest.Digest() != receipt.WorkspaceDigest {
		receipt.State, receipt.TargetState = FinalizationCompleted, orchestration.TaskFailed
		receipt.Reason += "; merged workspace changed before verification; original revision was not verified"
		return store.TransitionMergeFinalization(ctx, receipt, previous)
	}
	dispatched := receipt
	dispatched.State = FinalizationDispatched
	dispatched.TargetState, dispatched.Reason = "", ""
	if _, err := store.TransitionMergeFinalization(ctx, dispatched, previous); err != nil {
		return receipt, err
	}
	checkErr := executor.Verifier.VerifyMerge(ctx, applied)
	if ctx.Err() != nil {
		receipt.State, receipt.TargetState = FinalizationUnknown, orchestration.TaskFailed
		receipt.Reason += "; post-merge verification interrupted; outcome unknown, not rerun"
	} else {
		receipt.State = FinalizationCompleted
		if checkErr != nil {
			receipt.TargetState = orchestration.TaskFailed
			receipt.Reason += " but failed post-merge verification: " + checkErr.Error()
		} else {
			after, err := BuildManifest(ctx, executor.workspaceRoot(), nil)
			if err != nil || after.Digest() != receipt.WorkspaceDigest {
				receipt.TargetState = orchestration.TaskFailed
				receipt.Reason += "; workspace changed during post-merge verification"
			} else {
				receipt.Reason += " and passed post-merge verification of " + receipt.WorkspaceDigest
			}
		}
	}
	// A canceled verifier must still durably report that it cannot be replayed.
	durable, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalizationWriteTimeout)
	defer cancel()
	return store.TransitionMergeFinalization(durable, receipt, FinalizationDispatched)
}

func (executor *MergeExecutor) settleDelivered(ctx context.Context, applied MergeRequest, delivered []MergeRequest, target orchestration.TaskState, reason string) error {
	for _, merge := range delivered {
		if err := executor.finishAwaitingTask(ctx, merge.TaskID, target, applied.AgentID, reason); err != nil {
			return err
		}
		if executor.Coordinator == nil {
			continue
		}
		if merge.Kind == MergeSnapshot && target != orchestration.TaskCompleted {
			// Preserve the candidate for investigating a failed check.
			continue
		}
		if merge.Kind == MergeSnapshot && target == orchestration.TaskCompleted {
			if err := executor.Coordinator.DiscardSnapshotView(ctx, merge.ViewID, reason); err != nil {
				return err
			}
		} else if err := executor.Coordinator.FinalizeView(ctx, merge.ViewID, reason); err != nil {
			return err
		}
	}
	return nil
}

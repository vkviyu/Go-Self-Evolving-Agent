package sqlitestore

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

type finalizationVerifier func(context.Context, workspacecoord.MergeRequest) error

func (verify finalizationVerifier) VerifyMerge(ctx context.Context, merge workspacecoord.MergeRequest) error {
	return verify(ctx, merge)
}

type failTaskSettlement struct {
	orchestration.TaskRepository
	fail bool
}

func (tasks *failTaskSettlement) TransitionTask(ctx context.Context, transition orchestration.TaskTransition) (orchestration.TaskSnapshot, error) {
	if tasks.fail && transition.Target == orchestration.TaskCompleted {
		tasks.fail = false
		return orchestration.TaskSnapshot{}, errors.New("injected delivery failure")
	}
	return tasks.TaskRepository.TransitionTask(ctx, transition)
}

type failVerificationReceipt struct {
	workspacecoord.Repository
	workspacecoord.FinalizationStore
	fail bool
}

type failAppliedRecord struct {
	workspacecoord.Repository
	workspacecoord.FinalizationStore
	fail bool
}

func (store *failAppliedRecord) TransitionMerge(ctx context.Context, transition workspacecoord.MergeTransition) (workspacecoord.MergeRequest, error) {
	if store.fail && transition.Target == workspacecoord.MergeApplied {
		store.fail = false
		return workspacecoord.MergeRequest{}, errors.New("injected applied record failure")
	}
	return store.Repository.TransitionMerge(ctx, transition)
}

type identifiedVerifier struct {
	identity string
	verify   finalizationVerifier
}

func (verify identifiedVerifier) VerificationIdentity() string { return verify.identity }

func (verify identifiedVerifier) VerifyMerge(ctx context.Context, merge workspacecoord.MergeRequest) error {
	return verify.verify(ctx, merge)
}

func (store *failVerificationReceipt) TransitionMergeFinalization(ctx context.Context, receipt workspacecoord.MergeFinalization, previous workspacecoord.FinalizationState) (workspacecoord.MergeFinalization, error) {
	if store.fail && previous == workspacecoord.FinalizationDispatched {
		store.fail = false
		return workspacecoord.MergeFinalization{}, errors.New("injected verifier receipt failure")
	}
	return store.FinalizationStore.TransitionMergeFinalization(ctx, receipt, previous)
}

func finalizationWork(t *testing.T) (*snapshotMergeFixture, *snapshotWork) {
	t.Helper()
	fixture := newSnapshotMergeFixture(t, map[string]string{"a.txt": "base\n"})
	fixture.executor.AutoMerge = true
	work := fixture.start("finalization", "", nil)
	writeFixtureFile(t, filepath.Join(work.view.RootPath, "a.txt"), "updated\n")
	fixture.submit(work, "")
	return fixture, work
}

func TestAppliedMergeRecoversTaskDeliveryWithoutRepeatingVerification(t *testing.T) {
	fixture, work := finalizationWork(t)
	calls := 0
	fixture.executor.Verifier = finalizationVerifier(func(context.Context, workspacecoord.MergeRequest) error {
		calls++
		return nil
	})
	fixture.executor.Tasks = &failTaskSettlement{TaskRepository: fixture.store, fail: true}
	if _, err := fixture.executor.RunPending(fixture.ctx, fixture.root.TeamID, 0); err == nil {
		t.Fatal("missing injected task delivery failure")
	}
	fixture.expectMerge(work.merge.ID, workspacecoord.MergeApplied)
	fixture.expectTask(work.task.ID, orchestration.TaskAwaitingMerge)
	receipt, found, err := fixture.store.LoadMergeFinalization(fixture.ctx, work.merge.ID)
	if err != nil || !found || receipt.State != workspacecoord.FinalizationCompleted || receipt.Settled {
		t.Fatalf("interrupted receipt = %#v, found=%v, err=%v", receipt, found, err)
	}
	restarted, err := workspacecoord.NewMergeExecutor(fixture.store, fixture.coordinator, nil, fixture.artifacts, fixture.store)
	if err != nil {
		t.Fatal(err)
	}
	restarted.Verifier = finalizationVerifier(func(context.Context, workspacecoord.MergeRequest) error {
		calls++
		return errors.New("must not rerun")
	})
	// Reconciliation is independent of a new approval or current AutoMerge.
	if _, err := restarted.RunPending(fixture.ctx, fixture.root.TeamID, 0); err != nil {
		t.Fatal(err)
	}
	fixture.expectTask(work.task.ID, orchestration.TaskCompleted)
	if calls != 1 {
		t.Fatalf("verification calls = %d", calls)
	}
	receipt, _, err = fixture.store.LoadMergeFinalization(fixture.ctx, work.merge.ID)
	if err != nil || !receipt.Settled || receipt.WorkspaceDigest == "" {
		t.Fatalf("settled receipt = %#v, err=%v", receipt, err)
	}
}

func TestDispatchedVerificationRecoversUnknownWithoutReexecution(t *testing.T) {
	fixture, work := finalizationWork(t)
	wrapped := &failVerificationReceipt{Repository: fixture.store, FinalizationStore: fixture.store, fail: true}
	fixture.executor.Repository = wrapped
	calls := 0
	fixture.executor.Verifier = finalizationVerifier(func(context.Context, workspacecoord.MergeRequest) error {
		calls++
		return nil
	})
	if _, err := fixture.executor.RunPending(fixture.ctx, fixture.root.TeamID, 0); err == nil {
		t.Fatal("missing injected durable receipt failure")
	}
	receipt, _, err := fixture.store.LoadMergeFinalization(fixture.ctx, work.merge.ID)
	if err != nil || receipt.State != workspacecoord.FinalizationDispatched {
		t.Fatalf("receipt = %#v, err=%v", receipt, err)
	}
	restarted, err := workspacecoord.NewMergeExecutor(fixture.store, fixture.coordinator, nil, fixture.artifacts, fixture.store)
	if err != nil {
		t.Fatal(err)
	}
	restarted.Verifier = finalizationVerifier(func(context.Context, workspacecoord.MergeRequest) error {
		calls++
		return nil
	})
	if _, err := restarted.RunPending(fixture.ctx, fixture.root.TeamID, 0); err != nil {
		t.Fatal(err)
	}
	fixture.expectTask(work.task.ID, orchestration.TaskFailed)
	receipt, _, err = fixture.store.LoadMergeFinalization(fixture.ctx, work.merge.ID)
	if err != nil || receipt.State != workspacecoord.FinalizationUnknown || !receipt.Settled || calls != 1 {
		t.Fatalf("recovered receipt=%#v calls=%d err=%v", receipt, calls, err)
	}
}

func TestVerifierCannotPassWhenItChangesTheBoundWorkspace(t *testing.T) {
	fixture, work := finalizationWork(t)
	fixture.executor.Verifier = finalizationVerifier(func(context.Context, workspacecoord.MergeRequest) error {
		writeFixtureFile(t, filepath.Join(fixture.main, "a.txt"), "different revision\n")
		return nil
	})
	fixture.run()
	fixture.expectTask(work.task.ID, orchestration.TaskFailed)
	receipt, _, err := fixture.store.LoadMergeFinalization(fixture.ctx, work.merge.ID)
	if err != nil || !strings.Contains(receipt.Reason, "workspace changed") {
		t.Fatalf("receipt = %#v, err=%v", receipt, err)
	}
	fixture.expectView(work.view.ID, workspacecoord.ViewActive, true)
}

func TestPreparedVerificationCannotBeReplacedOrDisabledOnRestart(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "changed", true: "disabled"}[disabled], func(t *testing.T) {
			fixture, work := finalizationWork(t)
			calls := 0
			fixture.executor.Verifier = identifiedVerifier{identity: "check-A-timeout-1m", verify: func(context.Context, workspacecoord.MergeRequest) error {
				calls++
				return nil
			}}
			fixture.executor.Repository = &failAppliedRecord{Repository: fixture.store, FinalizationStore: fixture.store, fail: true}
			if _, err := fixture.executor.RunPending(fixture.ctx, fixture.root.TeamID, 0); err == nil {
				t.Fatal("missing applied-state fault")
			}
			fixture.expectMerge(work.merge.ID, workspacecoord.MergeRunning)
			restarted, err := workspacecoord.NewMergeExecutor(fixture.store, fixture.coordinator, nil, fixture.artifacts, fixture.store)
			if err != nil {
				t.Fatal(err)
			}
			restarted.AutoMerge = true
			if !disabled {
				restarted.Verifier = identifiedVerifier{identity: "check-B-timeout-2m", verify: func(context.Context, workspacecoord.MergeRequest) error {
					calls++
					return nil
				}}
			}
			if _, err := restarted.RunPending(fixture.ctx, fixture.root.TeamID, 0); err != nil {
				t.Fatal(err)
			}
			fixture.expectMerge(work.merge.ID, workspacecoord.MergeApplied)
			fixture.expectTask(work.task.ID, orchestration.TaskFailed)
			if calls != 0 {
				t.Fatalf("changed verifier ran %d times", calls)
			}
			receipt, _, err := fixture.store.LoadMergeFinalization(fixture.ctx, work.merge.ID)
			if err != nil || receipt.VerificationDigest != "check-A-timeout-1m" || !strings.Contains(receipt.Reason, "configuration changed") {
				t.Fatalf("receipt=%#v err=%v", receipt, err)
			}
		})
	}
}

func TestInterruptedVerifierPersistsUnknownAndRecoveryDoesNotRepeatIt(t *testing.T) {
	fixture, work := finalizationWork(t)
	ctx, cancel := context.WithCancel(fixture.ctx)
	defer cancel()
	calls := 0
	fixture.executor.Verifier = finalizationVerifier(func(context.Context, workspacecoord.MergeRequest) error {
		calls++
		cancel()
		return context.Canceled
	})
	if _, err := fixture.executor.RunPending(ctx, fixture.root.TeamID, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted execution error = %v", err)
	}
	receipt, _, err := fixture.store.LoadMergeFinalization(fixture.ctx, work.merge.ID)
	if err != nil || receipt.State != workspacecoord.FinalizationUnknown {
		t.Fatalf("canceled receipt=%#v err=%v", receipt, err)
	}
	if _, err := fixture.executor.RunPending(fixture.ctx, fixture.root.TeamID, 0); err != nil {
		t.Fatal(err)
	}
	fixture.expectTask(work.task.ID, orchestration.TaskFailed)
	if calls != 1 {
		t.Fatalf("interrupted check ran %d times", calls)
	}
}

func TestConflictTaskCreationIsReconciledAfterQueueTransition(t *testing.T) {
	fixture, work := finalizationWork(t)
	running, err := fixture.store.TransitionMerge(fixture.ctx, workspacecoord.MergeTransition{MergeID: work.merge.ID, Expected: work.merge.Version, Target: workspacecoord.MergeRunning})
	if err != nil {
		t.Fatal(err)
	}
	conflicted, err := fixture.store.TransitionMerge(fixture.ctx, workspacecoord.MergeTransition{MergeID: running.ID, Expected: running.Version, Target: workspacecoord.MergeConflicted, Reason: "simulated interruption"})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := fixture.executor.RunPending(fixture.ctx, fixture.root.TeamID, 0); err != nil {
			t.Fatal(err)
		}
	}
	conflicted = fixture.expectMerge(conflicted.ID, workspacecoord.MergeConflicted)
	if conflicted.ResolutionTaskID == "" {
		t.Fatal("resolution link was not repaired")
	}
	resolution, err := fixture.store.GetTask(fixture.ctx, conflicted.ResolutionTaskID)
	if err != nil || resolution.State != orchestration.TaskReady {
		t.Fatalf("resolution = %#v, err=%v", resolution, err)
	}
	tasks, err := fixture.store.ListTasks(fixture.ctx, orchestration.TaskQuery{TeamID: fixture.root.TeamID})
	if err != nil || len(tasks) != 2 {
		t.Fatalf("task count=%d err=%v", len(tasks), err)
	}
}

func TestDiscardCancelsApprovalWithoutWaitingForExecutorMutex(t *testing.T) {
	fixture, work := finalizationWork(t)
	fixture.executor.AutoMerge = false
	entered := make(chan struct{})
	fixture.executor.Approver = workspacecoord.MergeApproverFunc(func(ctx context.Context, _ workspacecoord.MergeApproval) (bool, error) {
		close(entered)
		<-ctx.Done()
		return false, ctx.Err()
	})
	done := make(chan error, 1)
	go func() {
		_, err := fixture.executor.RunPending(fixture.ctx, fixture.root.TeamID, 0)
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("approval was not requested")
	}
	ctx, cancel := context.WithTimeout(fixture.ctx, time.Second)
	defer cancel()
	merge, err := fixture.executor.Discard(ctx, work.merge.ID, "discard while approval waits")
	if err != nil || merge.State != workspacecoord.MergeRejected {
		t.Fatalf("discard = %#v, err=%v", merge, err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("approval cancellation error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("approval did not stop")
	}
	fixture.expectTask(work.task.ID, orchestration.TaskFailed)
	if got := readFixtureFile(t, filepath.Join(fixture.main, "a.txt")); got != "base\n" {
		t.Fatalf("discarded merge changed main: %q", got)
	}
}

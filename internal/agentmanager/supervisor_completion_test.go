package agentmanager

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/testfixture"
)

type completionTaskFault struct {
	*testfixture.OrchestrationStore
	fail atomic.Bool
}

func (fault *completionTaskFault) TransitionTask(ctx context.Context, transition orchestration.TaskTransition) (orchestration.TaskSnapshot, error) {
	if transition.Target == orchestration.TaskCompleted && fault.fail.CompareAndSwap(true, false) {
		return orchestration.TaskSnapshot{}, errors.New("injected coordinator settlement failure")
	}
	return fault.OrchestrationStore.TransitionTask(ctx, transition)
}

type completionMailboxFault struct {
	*testfixture.OrchestrationStore
	fail atomic.Bool
}

func (fault *completionMailboxFault) Enqueue(ctx context.Context, spec orchestration.MessageSpec) (orchestration.Message, error) {
	if spec.Type == orchestration.MessageResult && fault.fail.CompareAndSwap(true, false) {
		return orchestration.Message{}, errors.New("injected terminal hook failure")
	}
	return fault.OrchestrationStore.Enqueue(ctx, spec)
}

func completionFixture(t *testing.T, assigned bool) (*testfixture.Database, string, orchestration.RootThread, orchestration.AgentSnapshot, orchestration.TaskSnapshot) {
	t.Helper()
	ctx, path := context.Background(), filepath.Join(t.TempDir(), "completion.db")
	database, err := testfixture.Open(t, ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "completion-recovery"})
	if err != nil {
		t.Fatal(err)
	}
	var task orchestration.TaskSnapshot
	if assigned {
		task, err = store.CreateTask(ctx, orchestration.TaskSpec{TeamID: root.TeamID, Title: "Deliver feature"})
		if err != nil {
			t.Fatal(err)
		}
		task, err = store.TransitionTask(ctx, orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskReady})
		if err != nil {
			t.Fatal(err)
		}
	}
	spec := childSpec(root, "completion-worker", "Run exactly once")
	spec.TaskID = task.ID
	child, err := store.CreateChild(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	return database, path, root, child, task
}

func completionSupervisor(t *testing.T, config Config) *Supervisor {
	t.Helper()
	// Keep tests deterministic: recovery is requested explicitly, so a
	// background retry cannot hide the injected settlement failure.
	config.WaitPollInterval = time.Hour
	supervisor, err := NewSupervisor(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Close() })
	return supervisor
}

func completionPending(t *testing.T, store *testfixture.OrchestrationStore, team orchestration.TeamID, count int) []orchestration.AgentCompletion {
	t.Helper()
	pending, err := store.PendingAgentCompletions(context.Background(), team)
	if err != nil || len(pending) != count {
		t.Fatalf("pending completions=%+v want=%d err=%v", pending, count, err)
	}
	return pending
}

func TestCompletionRecoveryRetriesSettlementWithoutRerunningWorker(t *testing.T) {
	database, path, root, child, task := completionFixture(t, true)
	store, ctx := database.Orchestration(), context.Background()
	var calls atomic.Int32
	runner := RunnerFunc(func(context.Context, RunRequest) (orchestration.AgentResult, error) {
		calls.Add(1)
		return orchestration.AgentResult{Status: orchestration.ResultSucceeded, Summary: "work already done"}, nil
	})
	fault := &completionTaskFault{OrchestrationStore: store}
	fault.fail.Store(true)
	first := completionSupervisor(t, Config{Agents: store, Tasks: fault, Mailbox: store, Runner: runner})
	// Direct admission keeps shutdown separate from an explicit user cancel.
	// The durable worker was created before this Supervisor owned the Team.
	first.run(ctx, child)
	pending := completionPending(t, store, root.TeamID, 1)
	before, err := store.GetAgent(ctx, child.ID)
	if err != nil || before.State != orchestration.AgentRunning || pending[0].RunID != before.CurrentRunID {
		t.Fatalf("returned worker lost its pending identity: %+v %v", before, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := testfixture.Open(t, ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	store = reopened.Orchestration()
	second := completionSupervisor(t, Config{Agents: store, Tasks: store, Mailbox: store, Runner: runner})
	if err := second.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := store.GetAgent(ctx, child.ID)
	if err != nil || after.State != orchestration.AgentCompleted || after.CurrentRunID != before.CurrentRunID {
		t.Fatalf("worker did not settle its original Run: %+v %v", after, err)
	}
	completedTask, err := store.GetTask(ctx, task.ID)
	if err != nil || completedTask.State != orchestration.TaskCompleted {
		t.Fatalf("Task settlement failed: %+v %v", completedTask, err)
	}
	completionPending(t, store, root.TeamID, 0)
	if err := second.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("worker ran %d times after settlement retry", calls.Load())
	}
}

func TestCompletionRecoveryCompensatesTerminalHookAndFencesContinuation(t *testing.T) {
	database, path, root, child, _ := completionFixture(t, false)
	store, ctx := database.Orchestration(), context.Background()
	var calls atomic.Int32
	runner := RunnerFunc(func(context.Context, RunRequest) (orchestration.AgentResult, error) {
		calls.Add(1)
		return orchestration.AgentResult{Status: orchestration.ResultSucceeded, Summary: "done"}, nil
	})
	mailbox := &completionMailboxFault{OrchestrationStore: store}
	mailbox.fail.Store(true)
	first := completionSupervisor(t, Config{Agents: store, Mailbox: mailbox, Runner: runner})
	first.run(ctx, child)
	completionPending(t, store, root.TeamID, 1)
	terminal, err := store.GetAgent(ctx, child.ID)
	if err != nil || terminal.State != orchestration.AgentCompleted {
		t.Fatalf("hook failure rewrote the worker outcome: %+v %v", terminal, err)
	}
	if _, err := store.TransitionAgent(ctx, orchestration.AgentTransition{AgentID: child.ID, Expected: terminal.Version, Target: orchestration.AgentQueued, Activity: "continued"}); !errors.Is(err, orchestration.ErrAgentStateConflict) {
		t.Fatalf("continuation escaped unsettled terminal effects: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := testfixture.Open(t, ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	store = reopened.Orchestration()
	second := completionSupervisor(t, Config{Agents: store, Mailbox: store, Runner: runner})
	if err := second.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	completionPending(t, store, root.TeamID, 0)
	messages, err := store.ListMessages(ctx, orchestration.MessageQuery{TeamID: root.TeamID, RecipientAgentID: root.AgentID})
	if err != nil || len(messages) != 1 || messages[0].Type != orchestration.MessageResult {
		t.Fatalf("terminal notification was lost or duplicated: %+v %v", messages, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("terminal-hook recovery reran the worker %d times", calls.Load())
	}
	terminal, err = store.GetAgent(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionAgent(ctx, orchestration.AgentTransition{AgentID: child.ID, Expected: terminal.Version, Target: orchestration.AgentQueued, Activity: "continued"}); err != nil {
		t.Fatalf("settled terminal effects still block continuation: %v", err)
	}
}

func TestCompletionRejectsOldRunWithoutReleasingNewRunner(t *testing.T) {
	database, _, root, child, _ := completionFixture(t, false)
	ctx, store := context.Background(), database.Orchestration()
	oldRun, err := orchestration.NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	old, err := store.TransitionAgent(ctx, orchestration.AgentTransition{AgentID: child.ID, Expected: child.Version, Target: orchestration.AgentRunning, RunID: oldRun})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := store.TransitionAgent(ctx, orchestration.AgentTransition{AgentID: child.ID, Expected: old.Version, Target: orchestration.AgentQueued, Activity: "recovered"})
	if err != nil {
		t.Fatal(err)
	}
	newRun, err := orchestration.NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	current, err := store.TransitionAgent(ctx, orchestration.AgentTransition{AgentID: child.ID, Expected: queued.Version, Target: orchestration.AgentRunning, RunID: newRun})
	if err != nil {
		t.Fatal(err)
	}
	supervisor := completionSupervisor(t, Config{Agents: store})
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	supervisor.running[child.ID] = cancel
	if err := supervisor.finish(old, orchestration.AgentResult{Status: orchestration.ResultSucceeded, Summary: "late old result"}, nil); !errors.Is(err, orchestration.ErrAgentStateConflict) {
		t.Fatalf("stale result accepted: %v", err)
	}
	after, err := store.GetAgent(ctx, child.ID)
	if err != nil || after.CurrentRunID != current.CurrentRunID || after.Version != current.Version || !supervisor.hasRunner(child.ID) {
		t.Fatalf("old result changed the new Run: %+v %v", after, err)
	}
	completionPending(t, store, root.TeamID, 0)
}

func TestCompletionRecoveryWaitsForRefinementAndReleasesWorkerLeases(t *testing.T) {
	database, path, root, parent, parentTask := completionFixture(t, true)
	store, ctx := database.Orchestration(), context.Background()
	leases := newLeaseStore(t)
	var parentCalls, childCalls atomic.Int32
	var childTask orchestration.TaskSnapshot
	runner := RunnerFunc(func(ctx context.Context, request RunRequest) (orchestration.AgentResult, error) {
		result := orchestration.AgentResult{Status: orchestration.ResultSucceeded, Summary: "finished this worker's part"}
		if request.Agent.ID != parent.ID {
			childCalls.Add(1)
			_, err := leases.AcquireLease(ctx, orchestration.LeaseSpec{TeamID: root.TeamID, AgentID: request.Agent.ID, RunID: request.Agent.CurrentRunID, Path: "src/main.go", Mode: orchestration.LeaseWrite})
			return result, err
		}
		parentCalls.Add(1)
		if _, err := leases.AcquireLease(ctx, orchestration.LeaseSpec{TeamID: root.TeamID, AgentID: request.Agent.ID, RunID: request.Agent.CurrentRunID, Path: "src", Mode: orchestration.LeaseWrite}); err != nil {
			return orchestration.AgentResult{}, err
		}
		activeTask, err := store.GetTask(ctx, parentTask.ID)
		if err != nil {
			return orchestration.AgentResult{}, err
		}
		plan, err := store.RefineTasks(ctx, orchestration.TaskRefinementSpec{TeamID: root.TeamID, ActorAgentID: parent.ID, ParentTaskID: activeTask.ID, Expected: activeTask.Version, IdempotencyKey: "discovered-subtask", Children: []orchestration.TaskRefinementChild{{Key: "focused-work", Title: "Finish focused work"}}})
		if err != nil {
			return orchestration.AgentResult{}, err
		}
		childTask = plan.Children["focused-work"]
		childTask, err = store.TransitionTask(ctx, orchestration.TaskTransition{TaskID: childTask.ID, Expected: childTask.Version, Target: orchestration.TaskReady})
		return result, err
	})
	first := completionSupervisor(t, Config{Agents: store, Tasks: store, Mailbox: store, Runner: runner, Leases: leases})
	first.run(ctx, parent)
	pending := completionPending(t, store, root.TeamID, 1)
	before, err := store.GetAgent(ctx, parent.ID)
	if err != nil || before.State != orchestration.AgentRunning || pending[0].RunID != before.CurrentRunID {
		t.Fatalf("refined parent was completed or lost its receipt: %+v %v", before, err)
	}
	held, err := leases.ListLeases(ctx, orchestration.LeaseQuery{TeamID: root.TeamID, AgentID: parent.ID, States: []orchestration.LeaseState{orchestration.LeaseActive}})
	if err != nil || len(held) != 0 {
		t.Fatalf("returned parent still blocks refined child paths: %+v %v", held, err)
	}
	child, err := store.CreateChild(ctx, orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: parent.ID, TaskID: childTask.ID, ProfileRevision: "worker-v1", InitialPrompt: "Finish focused work"})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := testfixture.Open(t, ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	store = reopened.Orchestration()
	second := completionSupervisor(t, Config{Agents: store, Tasks: store, Mailbox: store, Runner: runner, Leases: leases})
	if err := second.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	waited, err := second.Wait(ctx, WaitCommand{AgentIDs: []orchestration.AgentID{child.ID}, Timeout: 2 * time.Second})
	if err != nil || !waited.Satisfied || waited.Agents[0].State != orchestration.AgentCompleted {
		t.Fatalf("refined child could not finish: %+v %v", waited, err)
	}
	if err := second.recoverPendingCompletions(ctx, root.TeamID); err != nil {
		t.Fatal(err)
	}
	after, err := store.GetAgent(ctx, parent.ID)
	if err != nil || after.State != orchestration.AgentCompleted || after.CurrentRunID != before.CurrentRunID {
		t.Fatalf("parent did not settle after its refined child: %+v %v", after, err)
	}
	completedTask, err := store.GetTask(ctx, parentTask.ID)
	if err != nil || completedTask.State != orchestration.TaskCompleted {
		t.Fatalf("refined parent Task did not complete: %+v %v", completedTask, err)
	}
	completionPending(t, store, root.TeamID, 0)
	if parentCalls.Load() != 1 || childCalls.Load() != 1 {
		t.Fatalf("unexpected execution count: parent=%d child=%d", parentCalls.Load(), childCalls.Load())
	}
}

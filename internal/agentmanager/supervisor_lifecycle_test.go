package agentmanager

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/sqlitestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

type cancelBeforeResultRepository struct {
	orchestration.AgentRepository
	once sync.Once
}

func (r *cancelBeforeResultRepository) TransitionAgent(ctx context.Context, transition orchestration.AgentTransition) (orchestration.AgentSnapshot, error) {
	if transition.Target == orchestration.AgentCanceling {
		var cancelErr error
		r.once.Do(func() {
			_, cancelErr = r.AgentRepository.TransitionAgent(ctx, orchestration.AgentTransition{
				AgentID: transition.AgentID, Expected: transition.Expected, Target: orchestration.AgentCanceling,
				Activity: "canceling", Reason: "injected concurrent shutdown",
			})
		})
		if cancelErr != nil {
			return orchestration.AgentSnapshot{}, cancelErr
		}
	}
	return r.AgentRepository.TransitionAgent(ctx, transition)
}

func TestCanceledResultSurvivesConcurrentCancelingTransition(t *testing.T) {
	t.Parallel()
	database, err := sqlitestore.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: "cancel-result-race"})
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := NewSupervisor(Config{
		Agents: &cancelBeforeResultRepository{AgentRepository: store},
		Runner: RunnerFunc(func(context.Context, RunRequest) (orchestration.AgentResult, error) {
			return orchestration.AgentResult{Status: orchestration.ResultCanceled, Summary: "durable cancellation evidence"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	child, err := supervisor.Spawn(context.Background(), childSpec(root, "cancel-race", "local fixture"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := supervisor.Wait(context.Background(), WaitCommand{AgentIDs: []orchestration.AgentID{child.ID}, Timeout: time.Second})
	if err != nil || !result.Satisfied {
		t.Fatalf("canceled result failed to settle: %+v, %v", result, err)
	}
	agent := result.Agents[0]
	if agent.State != orchestration.AgentCanceled || agent.Result == nil || agent.Result.Summary != "durable cancellation evidence" {
		t.Fatalf("canceled result lost at CAS boundary: %+v", agent)
	}
}

func TestConcurrentSupervisorClosePreservesTimeoutAndJoinsBeforeSuccess(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		name := "joined"
		if timeout {
			name = "timeout"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			started := make(chan struct{})
			canceled := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			runner := RunnerFunc(func(ctx context.Context, _ RunRequest) (orchestration.AgentResult, error) {
				close(started)
				<-ctx.Done()
				close(canceled)
				<-release
				return orchestration.AgentResult{Status: orchestration.ResultCanceled, Summary: "cancellation evidence"}, ctx.Err()
			})
			supervisor, store, root := newSupervisorTest(t, runner, 4, 3, 4)
			defer func() {
				once.Do(func() { close(release) })
				_ = supervisor.Close()
				supervisor.workers.Wait()
			}()
			child, err := supervisor.Spawn(context.Background(), childSpec(root, "closing", "wait for cancellation"))
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("runner did not start")
			}
			const callers = 8
			results := make(chan error, callers)
			for range callers {
				go func() { results <- supervisor.Close() }()
			}
			<-canceled
			if !timeout {
				once.Do(func() { close(release) })
			}
			for range callers {
				err := <-results
				if timeout && !errors.Is(err, ErrSupervisorCloseTimeout) || !timeout && err != nil {
					t.Errorf("timeout=%v, close result=%v", timeout, err)
				}
			}
			if timeout {
				if err := supervisor.Close(); !errors.Is(err, ErrSupervisorCloseTimeout) {
					t.Errorf("retry lost timeout: %v", err)
				}
				once.Do(func() { close(release) })
				supervisor.workers.Wait()
			}
			persisted, err := store.GetAgent(context.Background(), child.ID)
			if err != nil || persisted.State != orchestration.AgentCanceled || persisted.Result == nil || persisted.Result.Summary != "cancellation evidence" {
				t.Fatalf("worker completion not durable: %+v, %v", persisted, err)
			}
		})
	}
}

type cancelAwareAgentRepository struct {
	orchestration.AgentRepository
	listStarted chan struct{}
	listStopped chan struct{}
	started     sync.Once
	stopped     sync.Once
}

func (repository *cancelAwareAgentRepository) ListTeams(ctx context.Context) ([]orchestration.TeamID, error) {
	repository.started.Do(func() { close(repository.listStarted) })
	<-ctx.Done()
	repository.stopped.Do(func() { close(repository.listStopped) })
	return nil, ctx.Err()
}

func TestSupervisorCloseStopsWaitMonitor(t *testing.T) {
	database, err := sqlitestore.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	store := database.Orchestration()
	repository := &cancelAwareAgentRepository{
		AgentRepository: store,
		listStarted:     make(chan struct{}),
		listStopped:     make(chan struct{}),
	}
	supervisor, err := NewSupervisor(Config{
		Agents:           repository,
		WaitSets:         store,
		WaitPollInterval: time.Millisecond,
	})
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}

	select {
	case <-repository.listStarted:
	case <-time.After(time.Second):
		_ = supervisor.Close()
		_ = database.Close()
		t.Fatal("wait monitor did not start its repository query")
	}
	if err := supervisor.Close(); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	select {
	case <-repository.listStopped:
	default:
		_ = database.Close()
		t.Fatal("Supervisor closed before its wait monitor stopped")
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestInterruptFinalizesSuspendedAgent(t *testing.T) {
	supervisor, store, waiter := newSuspendedAgent(t)
	if err := supervisor.Interrupt(context.Background(), waiter.ID); err != nil {
		t.Fatal(err)
	}
	assertCanceledWithNoActiveView(t, store, waiter)
}

func TestCloseFinalizesSuspendedAgent(t *testing.T) {
	supervisor, store, waiter := newSuspendedAgent(t)
	if err := supervisor.Close(); err != nil {
		t.Fatal(err)
	}
	assertCanceledWithNoActiveView(t, store, waiter)
}

func TestRecoverFinalizesCancelingAgent(t *testing.T) {
	supervisor, store, waiter := newSuspendedAgent(t)
	persisted, err := store.GetAgent(context.Background(), waiter.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionAgent(context.Background(), orchestration.AgentTransition{
		AgentID: persisted.ID, Expected: persisted.Version, Target: orchestration.AgentCanceling, Activity: "canceling", Reason: "fixture crash",
	}); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertCanceledWithNoActiveView(t, store, waiter)
}

func newSuspendedAgent(t *testing.T) (*Supervisor, *sqlitestore.OrchestrationStore, orchestration.AgentSnapshot) {
	t.Helper()
	database, err := sqlitestore.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: "lifecycle-root"})
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := workspacecoord.NewCoordinator(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := NewSupervisor(Config{
		Agents: store, WaitSets: store, Workspace: coordinator, WorkspaceRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Close() })
	waiter, err := supervisor.Spawn(context.Background(), childSpec(root, "lifecycle-waiter", "wait"))
	if err != nil {
		t.Fatal(err)
	}
	target, err := store.CreateChild(context.Background(), childSpec(root, "lifecycle-target", "target"))
	if err != nil {
		t.Fatal(err)
	}
	runID, err := orchestration.NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	waiter, err = store.TransitionAgent(context.Background(), orchestration.AgentTransition{
		AgentID: waiter.ID, Expected: waiter.Version, Target: orchestration.AgentRunning, RunID: runID, Activity: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.startWorkspaceView(context.Background(), waiter); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Suspend(context.Background(), orchestration.WaitSpec{
		TeamID: waiter.TeamID, AgentID: waiter.ID, RunID: runID, Condition: "children", TargetAgentIDs: []orchestration.AgentID{target.ID},
	}); err != nil {
		t.Fatal(err)
	}
	return supervisor, store, waiter
}

func assertCanceledWithNoActiveView(t *testing.T, store *sqlitestore.OrchestrationStore, waiter orchestration.AgentSnapshot) {
	t.Helper()
	persisted, err := store.GetAgent(context.Background(), waiter.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != orchestration.AgentCanceled {
		t.Fatalf("suspended Agent state = %s, want canceled", persisted.State)
	}
	views, err := store.ListViews(context.Background(), workspacecoord.ViewQuery{
		TeamID: waiter.TeamID, AgentID: waiter.ID, States: []workspacecoord.ViewState{workspacecoord.ViewActive},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 0 {
		t.Fatalf("suspended Agent retained active workspace views: %#v", views)
	}
	waitSets, err := store.ListWaitSets(context.Background(), orchestration.WaitSetQuery{
		TeamID: waiter.TeamID, AgentID: waiter.ID, States: []orchestration.WaitSetState{orchestration.WaitSetPending},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(waitSets) != 0 {
		t.Fatalf("suspended Agent retained pending wait sets: %#v", waitSets)
	}
}

func TestRecoveryCancelsOrphanedQueuedDescendants(t *testing.T) {
	supervisor, store, root := newSupervisorTest(t, nil, 8, 4, 4)
	ctx := context.Background()
	parent, err := store.CreateChild(ctx, childSpec(root, "orphan-parent", "parent"))
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.CreateChild(ctx, orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: parent.ID, ProfileRevision: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after the parent committed terminal state, before its
	// recursive cleanup ran. No completion receipt exists for this transition.
	if _, err := store.TransitionAgent(ctx, orchestration.AgentTransition{AgentID: parent.ID, Expected: parent.Version, Target: orchestration.AgentFailed}); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	restored, err := store.GetAgent(ctx, child.ID)
	if err != nil || restored.State != orchestration.AgentCanceled {
		t.Fatalf("orphan left queued: %+v %v", restored, err)
	}
}

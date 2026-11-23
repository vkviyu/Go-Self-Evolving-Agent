package agentmanager

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/testfixture"
)

func newReassignmentSupervisorTest(t *testing.T, runner Runner) (*Supervisor, *testfixture.OrchestrationStore, orchestration.RootThread, session.EventStore) {
	t.Helper()
	database, err := testfixture.Open(t, context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: "retry-supervisor-root"})
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := NewSupervisor(Config{Agents: store, Tasks: store, Mailbox: store, Leases: newLeaseStore(t), Runner: runner, MaxRunningAgents: 1, MaxLiveAgents: 4, MaxDepth: 3, MaxChildren: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Close() })
	return supervisor, store, root, database.SessionEvents()
}

type unavailableFirstReassignment struct {
	orchestration.TaskRepository
	repository orchestration.TaskReassignmentRepository
	failed     atomic.Bool
}

func (repository *unavailableFirstReassignment) ReassignTask(ctx context.Context, id orchestration.TaskID, agent orchestration.AgentID, expected uint64, reason string) (orchestration.TaskSnapshot, error) {
	if !repository.failed.Swap(true) {
		return orchestration.TaskSnapshot{}, errors.New("injected reassignment storage interruption")
	}
	return repository.repository.ReassignTask(ctx, id, agent, expected, reason)
}

func retryJournalFailure(ctx context.Context, events session.EventStore, request RunRequest) error {
	_, err := events.Append(ctx, request.Agent.SessionID, session.AnySequence,
		session.PendingEvent{Type: session.EventTurnStarted, CorrelationID: "turn", Payload: session.MustJSONPayload(map[string]any{"turn_id": "turn", "run_id": request.Agent.CurrentRunID})},
		session.PendingEvent{Type: session.EventTurnFailed, CorrelationID: "turn", Payload: json.RawMessage(`{}`)})
	if err != nil {
		return err
	}
	return &orchestration.TaskAttemptFailure{Code: orchestration.TaskAttemptIterationLimit, Cause: errors.New("iteration limit")}
}

func TestSupervisorRecoverReassignsFrozenFailureAfterLostHook(t *testing.T) {
	var calls atomic.Int32
	var events session.EventStore
	runner := RunnerFunc(func(ctx context.Context, request RunRequest) (orchestration.AgentResult, error) {
		if calls.Add(1) == 1 {
			return orchestration.AgentResult{Status: orchestration.ResultFailed}, retryJournalFailure(ctx, events, request)
		}
		return orchestration.AgentResult{Status: orchestration.ResultSucceeded, Summary: "done"}, nil
	})
	supervisor, store, root, journal := newReassignmentSupervisorTest(t, runner)
	events = journal
	repository := &unavailableFirstReassignment{TaskRepository: store, repository: store}
	supervisor.tasks = repository
	task, err := store.CreateTask(context.Background(), orchestration.TaskSpec{TeamID: root.TeamID, Title: "retry after hook failure"})
	if err != nil {
		t.Fatal(err)
	}
	task, err = store.TransitionTask(context.Background(), orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskReady})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Spawn(context.Background(), orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, TaskID: task.ID, ProfileRevision: "worker-v1"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for !repository.failed.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !repository.failed.Load() {
		t.Fatal("failed attempt did not reach injected interruption")
	}
	if err := supervisor.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	for time.Now().Before(deadline) {
		current, _ := store.GetTask(context.Background(), task.ID)
		if current.State == orchestration.TaskCompleted {
			if current.Attempt != 1 || calls.Load() != 2 {
				t.Fatalf("attempt=%d calls=%d", current.Attempt, calls.Load())
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	current, _ := store.GetTask(context.Background(), task.ID)
	t.Fatalf("recovery did not dispatch new attempt: %#v calls=%d", current, calls.Load())
}

func TestSupervisorReassignmentStopsAfterThreeExecutions(t *testing.T) {
	var calls atomic.Int32
	var events session.EventStore
	runner := RunnerFunc(func(ctx context.Context, request RunRequest) (orchestration.AgentResult, error) {
		calls.Add(1)
		return orchestration.AgentResult{Status: orchestration.ResultFailed}, retryJournalFailure(ctx, events, request)
	})
	supervisor, store, root, journal := newReassignmentSupervisorTest(t, runner)
	events = journal
	task, err := store.CreateTask(context.Background(), orchestration.TaskSpec{TeamID: root.TeamID, Title: "bounded retry"})
	if err != nil {
		t.Fatal(err)
	}
	task, err = store.TransitionTask(context.Background(), orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskReady})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Spawn(context.Background(), orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, TaskID: task.ID, ProfileRevision: "worker-v1"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		current, _ := store.GetTask(context.Background(), task.ID)
		if current.State == orchestration.TaskFailed && current.Attempt == 2 && calls.Load() == 3 {
			if err := supervisor.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 3 {
				t.Fatalf("recovery exceeded attempt cap: %d", calls.Load())
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	current, _ := store.GetTask(context.Background(), task.ID)
	t.Fatalf("retry cap not reached safely: %#v calls=%d", current, calls.Load())
}

func TestTaskAttemptRetryClassificationUsesTrustedError(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		unknown bool
		want    bool
	}{
		{"runner boolean alone", errors.New("please retry"), false, false},
		{"success cannot retry", nil, false, false},
		{"typed iteration limit", &orchestration.TaskAttemptFailure{Code: orchestration.TaskAttemptIterationLimit, Cause: errors.New("limit")}, false, true},
		{"unknown code", &orchestration.TaskAttemptFailure{Code: "model_said_retry", Cause: errors.New("limit")}, false, false},
		{"unknown outcome", &orchestration.TaskAttemptFailure{Code: orchestration.TaskAttemptIterationLimit, Cause: errors.New("limit")}, true, false},
		{"provider pause wins", &orchestration.ExecutionStop{Kind: orchestration.ExecutionQuota, Cause: &orchestration.TaskAttemptFailure{Code: orchestration.TaskAttemptIterationLimit, Cause: errors.New("limit")}}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := orchestration.AgentResult{Retryable: true, RetryCode: orchestration.TaskAttemptIterationLimit, OutcomeUnknown: tc.unknown}
			classifyTaskAttemptFailure(&result, tc.err)
			if result.Retryable != tc.want || (result.RetryCode != "") != tc.want {
				t.Fatalf("classification=%#v", result)
			}
		})
	}
}

func TestSupervisorReassignmentChecksAuthoritativeWorkspaceLease(t *testing.T) {
	supervisor, store, root, _ := newReassignmentSupervisorTest(t, nil)
	ctx := context.Background()
	agent, err := store.CreateChild(ctx, childSpec(root, "lease-proof", "inspect"))
	if err != nil {
		t.Fatal(err)
	}
	result := orchestration.AgentResult{Status: orchestration.ResultFailed}
	agent, err = store.TransitionAgent(ctx, orchestration.AgentTransition{AgentID: agent.ID, Expected: agent.Version, Target: orchestration.AgentRunning})
	if err != nil {
		t.Fatal(err)
	}
	agent, err = store.TransitionAgent(ctx, orchestration.AgentTransition{AgentID: agent.ID, Expected: agent.Version, Target: orchestration.AgentFailed, Result: &result})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := supervisor.leases.AcquireLease(ctx, orchestration.LeaseSpec{TeamID: root.TeamID, AgentID: agent.ID, Path: "output.txt", Mode: orchestration.LeaseWrite})
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.proveReassignmentQuiescent(ctx, "", agent.ID); !errors.Is(err, orchestration.ErrTaskReassignUnsafe) {
		t.Fatalf("active cross-domain lease allowed: %v", err)
	}
	if _, err := supervisor.leases.ReleaseLease(ctx, lease.ID, agent.ID, "test execution exited"); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.proveReassignmentQuiescent(ctx, "", agent.ID); err != nil {
		t.Fatalf("released cross-domain lease rejected: %v", err)
	}
}

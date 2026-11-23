package agentmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentprofile"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/testfixture"
	appTool "github.com/vkviyu/go-self-evolving-agent/internal/tool"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacedomain"
)

type testRunner struct {
	mu       sync.Mutex
	requests []RunRequest
	started  chan RunRequest
	result   orchestration.AgentResult
}

type invalidResultRunner struct{}

type resumeRunner struct {
	mu       sync.Mutex
	requests []RunRequest
	started  chan RunRequest
	target   orchestration.AgentID
}

func (runner *resumeRunner) Run(context.Context, RunRequest) (orchestration.AgentResult, error) {
	return orchestration.AgentResult{}, errors.New("legacy runner should not be called")
}

func (runner *resumeRunner) RunWithSuspension(_ context.Context, request RunRequest) (RunnerOutcome, error) {
	runner.mu.Lock()
	runner.requests = append(runner.requests, request)
	callNumber := len(runner.requests)
	runner.mu.Unlock()
	if runner.started != nil {
		runner.started <- request
	}
	if callNumber == 1 {
		return RunnerOutcome{Wait: &orchestration.WaitSpec{Condition: "child-ready", TargetAgentIDs: []orchestration.AgentID{runner.target}, ResumeToken: "checkpoint-turn-1", IdempotencyKey: "wait-resume-1"}}, nil
	}
	return RunnerOutcome{Result: orchestration.AgentResult{Status: orchestration.ResultSucceeded, Summary: "resumed"}}, nil
}

func (invalidResultRunner) Run(context.Context, RunRequest) (orchestration.AgentResult, error) {
	return orchestration.AgentResult{}, nil
}

func (runner *testRunner) Run(ctx context.Context, request RunRequest) (orchestration.AgentResult, error) {
	runner.mu.Lock()
	runner.requests = append(runner.requests, request)
	runner.mu.Unlock()
	if runner.started != nil {
		runner.started <- request
	}
	if runner.result.Status != "" {
		return runner.result, nil
	}
	<-ctx.Done()
	return orchestration.AgentResult{Status: orchestration.ResultCanceled, Error: ctx.Err().Error()}, ctx.Err()
}

func newSupervisorTest(t *testing.T, runner Runner, maxLive, maxDepth, maxChildren int) (*Supervisor, *testfixture.OrchestrationStore, orchestration.RootThread) {
	return newSupervisorTestWithRunning(t, runner, 1, maxLive, maxDepth, maxChildren)
}

func newSupervisorTestWithRunning(t *testing.T, runner Runner, maxRunning, maxLive, maxDepth, maxChildren int) (*Supervisor, *testfixture.OrchestrationStore, orchestration.RootThread) {
	t.Helper()
	database, err := testfixture.Open(t, context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: "supervisor-root"})
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := NewSupervisor(Config{
		Agents: store, Mailbox: store, Leases: newLeaseStore(t), Runner: runner, MaxRunningAgents: maxRunning, MaxLiveAgents: maxLive, MaxDepth: maxDepth, MaxChildren: maxChildren,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Close() })
	return supervisor, store, root
}

// newLeaseStore opens a coordination domain over a private workspace. The
// domain closes after the Supervisor because cleanups run in reverse order.
func newLeaseStore(t *testing.T) *workspacedomain.LeaseStore {
	t.Helper()
	domain, err := workspacedomain.Open(context.Background(), workspacedomain.Options{HomeDirectory: t.TempDir(), WorkspaceRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = domain.Close() })
	return domain.Leases()
}

func childSpec(root orchestration.RootThread, key, prompt string) orchestration.SpawnSpec {
	return orchestration.SpawnSpec{
		TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker-v1",
		IdempotencyKey: key, InitialPrompt: prompt,
	}
}

func TestSupervisorRunsChildAndPersistsResult(t *testing.T) {
	runner := &testRunner{result: orchestration.AgentResult{Status: orchestration.ResultSucceeded, Summary: "done"}}
	supervisor, store, root := newSupervisorTest(t, runner, 4, 3, 4)
	child, err := supervisor.Spawn(context.Background(), childSpec(root, "run-once", "inspect the repository"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := supervisor.Wait(context.Background(), WaitCommand{AgentIDs: []orchestration.AgentID{child.ID}, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Satisfied || result.Agents[0].State != orchestration.AgentCompleted {
		t.Fatalf("unexpected wait result: %#v", result)
	}
	persisted, err := store.GetAgent(context.Background(), child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.CurrentRunID == "" || persisted.InitialPrompt != "inspect the repository" {
		t.Fatalf("durable run metadata missing: %#v", persisted)
	}
	messages, err := store.ListMessages(context.Background(), orchestration.MessageQuery{TeamID: root.TeamID, RecipientAgentID: root.AgentID, States: []orchestration.DeliveryState{orchestration.DeliveryEnqueued}})
	if err != nil || len(messages) != 1 || messages[0].Type != orchestration.MessageResult {
		t.Fatalf("terminal result was not atomically delivered once: %#v %v", messages, err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.requests) != 1 || runner.requests[0].Prompt != "inspect the repository" {
		t.Fatalf("unexpected runner request: %#v", runner.requests)
	}
}

func TestRecoverableRunnerFailurePausesAndResumesSameRun(t *testing.T) {
	var calls sync.Mutex
	var requests []RunRequest
	runner := RunnerFunc(func(_ context.Context, request RunRequest) (orchestration.AgentResult, error) {
		calls.Lock()
		requests = append(requests, request)
		calls.Unlock()
		return orchestration.AgentResult{Status: orchestration.ResultFailed, Error: "provider unavailable"}, &orchestration.ExecutionStop{Kind: orchestration.ExecutionProviderUnavailable, Cause: errors.New("provider unavailable")}
	})
	supervisor, store, root := newSupervisorTest(t, runner, 4, 3, 4)
	child, err := supervisor.Spawn(context.Background(), childSpec(root, "recoverable-provider", "continue later"))
	if err != nil {
		t.Fatal(err)
	}
	var paused orchestration.AgentSnapshot
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		paused, err = store.GetAgent(context.Background(), child.ID)
		if err == nil && paused.State == orchestration.AgentWaiting {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if paused.State != orchestration.AgentWaiting || paused.Activity != "recovery:provider_unavailable" {
		t.Fatalf("recoverable failure became terminal: %#v", paused)
	}
	resumed, err := supervisor.ResumeAgent(context.Background(), child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.CurrentRunID != paused.CurrentRunID || resumed.State != orchestration.AgentQueued {
		t.Fatalf("resume changed execution identity: before=%#v after=%#v", paused, resumed)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		current, _ := store.GetAgent(context.Background(), child.ID)
		if current.State == orchestration.AgentWaiting && current.Activity == "recovery:provider_unavailable" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	calls.Lock()
	defer calls.Unlock()
	if len(requests) != 2 || requests[0].Agent.CurrentRunID != requests[1].Agent.CurrentRunID {
		t.Fatalf("resume did not reuse original run: %#v", requests)
	}
	if requests[0].Recover || !requests[1].Recover || requests[1].Prompt != "" || len(requests[1].Messages) != 0 {
		t.Fatalf("resume did not route the original run to recovery: %#v", requests)
	}
}

func TestSupervisorCompletesAssignedTaskWithChild(t *testing.T) {
	runner := &testRunner{result: orchestration.AgentResult{Status: orchestration.ResultSucceeded, Summary: "implemented"}}
	supervisor, store, root := newSupervisorTest(t, runner, 4, 3, 4)
	supervisor.tasks = store
	task, err := store.CreateTask(context.Background(), orchestration.TaskSpec{TeamID: root.TeamID, Title: "Implement feature"})
	if err != nil {
		t.Fatal(err)
	}
	task, err = store.TransitionTask(context.Background(), orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskReady})
	if err != nil {
		t.Fatal(err)
	}
	child, err := supervisor.Spawn(context.Background(), orchestration.SpawnSpec{
		TeamID: root.TeamID, ParentAgentID: root.AgentID, TaskID: task.ID, ProfileRevision: "worker-v1", InitialPrompt: "implement it",
	})
	if err != nil {
		t.Fatal(err)
	}
	waited, err := supervisor.Wait(context.Background(), WaitCommand{AgentIDs: []orchestration.AgentID{child.ID}, Timeout: time.Second})
	if err != nil || !waited.Satisfied || waited.Agents[0].State != orchestration.AgentCompleted {
		t.Fatalf("child did not complete: result=%#v err=%v", waited, err)
	}
	completed, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != orchestration.TaskCompleted || completed.AssigneeAgentID != child.ID {
		t.Fatalf("assigned task was not completed: %#v", completed)
	}
}

func TestSupervisorFailsAssignedTaskWithChild(t *testing.T) {
	runner := &testRunner{result: orchestration.AgentResult{Status: orchestration.ResultFailed, Error: "build failed"}}
	supervisor, store, root := newSupervisorTest(t, runner, 4, 3, 4)
	supervisor.tasks = store
	task, err := store.CreateTask(context.Background(), orchestration.TaskSpec{TeamID: root.TeamID, Title: "Build"})
	if err != nil {
		t.Fatal(err)
	}
	task, err = store.TransitionTask(context.Background(), orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskReady})
	if err != nil {
		t.Fatal(err)
	}
	child, err := supervisor.Spawn(context.Background(), orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, TaskID: task.ID, ProfileRevision: "worker-v1"})
	if err != nil {
		t.Fatal(err)
	}
	waited, err := supervisor.Wait(context.Background(), WaitCommand{AgentIDs: []orchestration.AgentID{child.ID}, Timeout: time.Second})
	if err != nil || !waited.Satisfied || waited.Agents[0].State != orchestration.AgentFailed {
		t.Fatalf("child did not fail: result=%#v err=%v", waited, err)
	}
	failed, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.State != orchestration.TaskFailed {
		t.Fatalf("assigned task was not failed: %#v", failed)
	}
}

func TestSupervisorReassignsOnlyExplicitlyRetryableFailure(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	var events session.EventStore
	runner := RunnerFunc(func(ctx context.Context, request RunRequest) (orchestration.AgentResult, error) {
		mu.Lock()
		calls++
		current := calls
		mu.Unlock()
		if current == 1 {
			_, err := events.Append(ctx, request.Agent.SessionID, session.AnySequence,
				session.PendingEvent{Type: session.EventTurnStarted, CorrelationID: "turn", Payload: session.MustJSONPayload(map[string]any{"turn_id": "turn", "run_id": request.Agent.CurrentRunID})},
				session.PendingEvent{Type: session.EventTurnFailed, CorrelationID: "turn", Payload: json.RawMessage(`{}`)})
			if err != nil {
				return orchestration.AgentResult{}, err
			}
			return orchestration.AgentResult{Status: orchestration.ResultFailed}, &orchestration.TaskAttemptFailure{Code: orchestration.TaskAttemptIterationLimit, Cause: errors.New("iteration limit")}
		}
		return orchestration.AgentResult{Status: orchestration.ResultSucceeded, Summary: "reassigned complete"}, nil
	})
	supervisor, store, root, journal := newReassignmentSupervisorTest(t, runner)
	events = journal
	supervisor.tasks = store
	task, err := store.CreateTask(context.Background(), orchestration.TaskSpec{TeamID: root.TeamID, Title: "retryable task"})
	if err != nil {
		t.Fatal(err)
	}
	task, err = store.TransitionTask(context.Background(), orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskReady})
	if err != nil {
		t.Fatal(err)
	}
	first, err := supervisor.Spawn(context.Background(), orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, TaskID: task.ID, ProfileRevision: "worker-v1", InitialPrompt: "retry"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		current, _ := store.GetTask(context.Background(), task.ID)
		if current.State == orchestration.TaskCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	finalTask, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finalTask.State != orchestration.TaskCompleted || finalTask.Attempt != 1 || finalTask.AssigneeAgentID == first.ID {
		t.Fatalf("retryable task was not reassigned safely: %#v", finalTask)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("runner calls=%d, want one retry", calls)
	}
}

func TestSupervisorReleasesAssignedTaskOnCancellation(t *testing.T) {
	runner := &testRunner{started: make(chan RunRequest, 1)}
	supervisor, store, root := newSupervisorTest(t, runner, 4, 3, 4)
	supervisor.tasks = store
	task, err := store.CreateTask(context.Background(), orchestration.TaskSpec{TeamID: root.TeamID, Title: "Cancelable work"})
	if err != nil {
		t.Fatal(err)
	}
	task, err = store.TransitionTask(context.Background(), orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskReady})
	if err != nil {
		t.Fatal(err)
	}
	child, err := supervisor.Spawn(context.Background(), orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, TaskID: task.ID, ProfileRevision: "worker-v1"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("child did not start")
	}
	if err := supervisor.Interrupt(context.Background(), child.ID); err != nil {
		t.Fatal(err)
	}
	waited, err := supervisor.Wait(context.Background(), WaitCommand{AgentIDs: []orchestration.AgentID{child.ID}, Timeout: time.Second})
	if err != nil || !waited.Satisfied || waited.Agents[0].State != orchestration.AgentCanceled {
		t.Fatalf("child was not canceled: result=%#v err=%v", waited, err)
	}
	released, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if released.State != orchestration.TaskReady || released.AssigneeAgentID != "" || released.Attempt != 1 {
		t.Fatalf("assigned task was not released for retry: %#v", released)
	}
}

func TestSupervisorDispatchesReadyTaskAutomatically(t *testing.T) {
	runner := &testRunner{result: orchestration.AgentResult{Status: orchestration.ResultSucceeded, Summary: "task done"}}
	supervisor, store, root := newSupervisorTest(t, runner, 4, 3, 4)
	supervisor.tasks = store
	task, err := store.CreateTask(context.Background(), orchestration.TaskSpec{
		TeamID: root.TeamID, Title: "Automatically dispatched", Description: "run this task", Priority: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.TransitionTask(context.Background(), orchestration.TaskTransition{
		TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskReady, ActorAgentID: root.AgentID,
	}); err != nil {
		t.Fatal(err)
	}
	agents, err := store.ListAgents(context.Background(), orchestration.AgentQuery{TeamID: root.TeamID, ParentAgentID: root.AgentID})
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].AssignedTaskID != task.ID {
		t.Fatalf("ready task was not dispatched: %#v", agents)
	}
	waited, err := supervisor.Wait(context.Background(), WaitCommand{AgentIDs: []orchestration.AgentID{agents[0].ID}, Timeout: time.Second})
	if err != nil || !waited.Satisfied || waited.Agents[0].State != orchestration.AgentCompleted {
		t.Fatalf("dispatched child did not complete: result=%#v err=%v", waited, err)
	}
	completed, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != orchestration.TaskCompleted {
		t.Fatalf("dispatched task state = %s, want completed", completed.State)
	}
}

func TestSupervisorDoesNotCompleteTaskForBlockedOrPartialGoal(t *testing.T) {
	for _, test := range []struct {
		name   string
		result orchestration.AgentResult
	}{
		{name: "blocked", result: orchestration.AgentResult{Status: orchestration.ResultBlocked, Summary: "cannot continue", Blockers: []string{"external credential missing"}}},
		{name: "partial", result: orchestration.AgentResult{Status: orchestration.ResultPartial, Summary: "best effort", Risks: []string{"round budget exhausted"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &testRunner{result: test.result}
			supervisor, store, root := newSupervisorTest(t, runner, 4, 3, 4)
			supervisor.tasks = store
			task, err := store.CreateTask(context.Background(), orchestration.TaskSpec{TeamID: root.TeamID, Title: "Outcome projection"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := supervisor.TransitionTask(context.Background(), orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskReady, ActorAgentID: root.AgentID}); err != nil {
				t.Fatal(err)
			}
			agents, err := store.ListAgents(context.Background(), orchestration.AgentQuery{TeamID: root.TeamID, ParentAgentID: root.AgentID})
			if err != nil || len(agents) != 1 {
				t.Fatalf("dispatched agents=%#v err=%v", agents, err)
			}
			waited, err := supervisor.Wait(context.Background(), WaitCommand{AgentIDs: []orchestration.AgentID{agents[0].ID}, Timeout: time.Second})
			if err != nil || !waited.Satisfied || waited.Agents[0].Result == nil || waited.Agents[0].Result.Status != test.result.Status {
				t.Fatalf("Agent outcome=%#v err=%v", waited, err)
			}
			blocked, err := store.GetTask(context.Background(), task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if blocked.State != orchestration.TaskBlocked || blocked.AssigneeAgentID != "" {
				t.Fatalf("task outcome = %#v, want unassigned blocked", blocked)
			}
		})
	}
}

func TestSupervisorRecoveryDispatchesDurableReadyTask(t *testing.T) {
	database, err := testfixture.Open(t, context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: "recovery-task-root"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(context.Background(), orchestration.TaskSpec{TeamID: root.TeamID, Title: "Recoverable task"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionTask(context.Background(), orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskReady}); err != nil {
		t.Fatal(err)
	}
	runner := &testRunner{result: orchestration.AgentResult{Status: orchestration.ResultSucceeded, Summary: "recovered task done"}}
	supervisor, err := NewSupervisor(Config{Agents: store, Tasks: store, Mailbox: store, Runner: runner, MaxRunningAgents: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Close() })
	if err := supervisor.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	agents, err := store.ListAgents(context.Background(), orchestration.AgentQuery{TeamID: root.TeamID, ParentAgentID: root.AgentID})
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].AssignedTaskID != task.ID {
		t.Fatalf("recovery did not dispatch ready task: %#v", agents)
	}
	waited, err := supervisor.Wait(context.Background(), WaitCommand{AgentIDs: []orchestration.AgentID{agents[0].ID}, Timeout: time.Second})
	if err != nil || !waited.Satisfied {
		t.Fatalf("recovered task child did not finish: result=%#v err=%v", waited, err)
	}
	completed, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != orchestration.TaskCompleted {
		t.Fatalf("recovered task state = %s, want completed", completed.State)
	}
}

func TestSupervisorDispatchesDependentTaskAfterCompletion(t *testing.T) {
	runner := &testRunner{result: orchestration.AgentResult{Status: orchestration.ResultSucceeded, Summary: "step complete"}}
	supervisor, store, root := newSupervisorTest(t, runner, 2, 6, 4)
	supervisor.tasks = store
	first, err := store.CreateTask(context.Background(), orchestration.TaskSpec{TeamID: root.TeamID, Title: "Prepare"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateTask(context.Background(), orchestration.TaskSpec{TeamID: root.TeamID, Title: "Consume"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddDependency(context.Background(), second.ID, first.ID, root.AgentID, "dependency-chain"); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.TransitionTask(context.Background(), orchestration.TaskTransition{TaskID: first.ID, Expected: first.Version, Target: orchestration.TaskReady, ActorAgentID: root.AgentID}); err != nil {
		t.Fatal(err)
	}
	firstAgents, err := store.ListAgents(context.Background(), orchestration.AgentQuery{TeamID: root.TeamID, ParentAgentID: root.AgentID})
	if err != nil {
		t.Fatal(err)
	}
	if len(firstAgents) != 1 || firstAgents[0].AssignedTaskID != first.ID {
		t.Fatalf("first task was not dispatched: %#v", firstAgents)
	}
	firstWait, err := supervisor.Wait(context.Background(), WaitCommand{AgentIDs: []orchestration.AgentID{firstAgents[0].ID}, Timeout: time.Second})
	if err != nil || !firstWait.Satisfied {
		t.Fatalf("first task did not complete: result=%#v err=%v", firstWait, err)
	}
	secondTask, err := store.GetTask(context.Background(), second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if secondTask.State != orchestration.TaskClaimed && secondTask.State != orchestration.TaskInProgress && secondTask.State != orchestration.TaskCompleted {
		t.Fatalf("dependent task was not dispatched after completion: %#v", secondTask)
	}
	agents, err := store.ListAgents(context.Background(), orchestration.AgentQuery{TeamID: root.TeamID, ParentAgentID: root.AgentID})
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 2 {
		t.Fatalf("dependent task child count = %d, want 2: %#v", len(agents), agents)
	}
	secondAgent := agents[1]
	if secondAgent.AssignedTaskID != second.ID {
		secondAgent = agents[0]
	}
	if secondAgent.AssignedTaskID != second.ID {
		t.Fatalf("dependent task child missing: %#v", agents)
	}
	secondWait, err := supervisor.Wait(context.Background(), WaitCommand{AgentIDs: []orchestration.AgentID{secondAgent.ID}, Timeout: time.Second})
	if err != nil || !secondWait.Satisfied {
		t.Fatalf("dependent task child did not complete: result=%#v err=%v", secondWait, err)
	}
	completed, err := store.GetTask(context.Background(), second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != orchestration.TaskCompleted {
		t.Fatalf("dependent task state = %s, want completed", completed.State)
	}
}

func TestTaskVerifyToolRequiresReviewerProfile(t *testing.T) {
	database, err := testfixture.Open(t, context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: "task-verify-tool-root"})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := store.CreateChild(context.Background(), orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker-v1"})
	if err != nil {
		t.Fatal(err)
	}
	otherReviewer, err := store.CreateChild(context.Background(), orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "reviewer-v1"})
	if err != nil {
		t.Fatal(err)
	}
	profiles := agentprofile.NewRegistry()
	limits := agentprofile.Limits{MaxIterations: 1, MaxToolCalls: 1, MaxWallTime: time.Minute}
	if _, err := profiles.Activate(
		agentprofile.Profile{Name: "worker", Revision: "worker-v1", ModelProfile: "test", ContextInheritance: agentprofile.InheritSummary, WorkspaceMode: agentprofile.WorkspaceSharedLease, Limits: limits, CanManageTasks: true},
		agentprofile.Profile{Name: "reviewer", Revision: "reviewer-v1", ModelProfile: "test", ContextInheritance: agentprofile.InheritSummary, WorkspaceMode: agentprofile.WorkspaceSharedReadonly, Limits: limits, CanManageTasks: true},
	); err != nil {
		t.Fatal(err)
	}
	supervisor, err := NewSupervisor(Config{Agents: store, Tasks: store, Profiles: profiles})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Close() })
	task, err := store.CreateTask(context.Background(), orchestration.TaskSpec{TeamID: root.TeamID, Title: "Verify via tool", VerificationProfile: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	task, err = store.TransitionTask(context.Background(), orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskReady})
	if err != nil {
		t.Fatal(err)
	}
	task, err = store.ClaimTask(context.Background(), task.ID, worker.ID, task.Version)
	if err != nil {
		t.Fatal(err)
	}
	task, err = store.TransitionTask(context.Background(), orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskInProgress, AssigneeAgentID: worker.ID, ActorAgentID: worker.ID})
	if err != nil {
		t.Fatal(err)
	}
	task, err = store.TransitionTask(context.Background(), orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskVerification, AssigneeAgentID: worker.ID, ActorAgentID: worker.ID})
	if err != nil {
		t.Fatal(err)
	}
	bindings := Bindings(supervisor)
	var verify appTool.Binding
	for _, binding := range bindings {
		if binding.Descriptor.Name == "task_verify" {
			verify = binding
		}
	}
	wrongContext := orchestration.WithActorScope(context.Background(), worker.ActorScope())
	if _, err := verify.Executor.Invoke(wrongContext, json.RawMessage(`{"task_id":"`+string(task.ID)+`","passed":true}`)); !errors.Is(err, orchestration.ErrTaskReviewerRequired) {
		t.Fatalf("worker verify got %v, want reviewer required", err)
	}
	otherContext := orchestration.WithActorScope(context.Background(), otherReviewer.ActorScope())
	if _, err := verify.Executor.Invoke(otherContext, json.RawMessage(`{"task_id":"`+string(task.ID)+`","passed":true}`)); !errors.Is(err, orchestration.ErrTaskReviewerRequired) {
		t.Fatalf("unassigned reviewer verify got %v, want reviewer required", err)
	}
	reviewer, err := store.CreateChild(context.Background(), orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "reviewer-v1", IdempotencyKey: reviewKey(task)})
	if err != nil {
		t.Fatal(err)
	}
	reviewerContext := orchestration.WithActorScope(context.Background(), reviewer.ActorScope())
	rejected, err := verify.Executor.Invoke(reviewerContext, json.RawMessage(`{"task_id":"`+string(task.ID)+`","passed":false,"reason":"needs correction"}`))
	if err != nil {
		t.Fatalf("designated reviewer could not request correction: %v", err)
	}
	previousAttempt := task.Attempt
	if err := json.Unmarshal(rejected, &task); err != nil || task.State != orchestration.TaskReady || task.Attempt != previousAttempt+1 {
		t.Fatalf("rejected review = %s, err=%v", rejected, err)
	}
	task, err = store.ClaimTask(context.Background(), task.ID, worker.ID, task.Version)
	if err != nil {
		t.Fatal(err)
	}
	task, err = store.TransitionTask(context.Background(), orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskInProgress, AssigneeAgentID: worker.ID, ActorAgentID: worker.ID})
	if err != nil {
		t.Fatal(err)
	}
	task, err = store.TransitionTask(context.Background(), orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskVerification, AssigneeAgentID: worker.ID, ActorAgentID: worker.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verify.Executor.Invoke(reviewerContext, json.RawMessage(`{"task_id":"`+string(task.ID)+`","passed":true}`)); !errors.Is(err, orchestration.ErrTaskReviewerRequired) {
		t.Fatalf("old-attempt reviewer accepted: %v", err)
	}
	reviewer, err = store.CreateChild(context.Background(), orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "reviewer-v1", IdempotencyKey: reviewKey(task)})
	if err != nil {
		t.Fatal(err)
	}
	reviewerContext = orchestration.WithActorScope(context.Background(), reviewer.ActorScope())
	result, err := verify.Executor.Invoke(reviewerContext, json.RawMessage(`{"task_id":"`+string(task.ID)+`","passed":true}`))
	if err != nil {
		t.Fatal(err)
	}
	var completed orchestration.TaskSnapshot
	if err := json.Unmarshal(result, &completed); err != nil || completed.State != orchestration.TaskCompleted {
		t.Fatalf("invalid verification result: %s (%v)", result, err)
	}
}

func TestSupervisorMovesSuccessfulWorkerToVerificationAndSpawnsReviewer(t *testing.T) {
	runner := &testRunner{result: orchestration.AgentResult{Status: orchestration.ResultSucceeded, Summary: "implemented"}}
	supervisor, store, root := newSupervisorTest(t, runner, 4, 4, 4)
	supervisor.tasks = store
	task, err := store.CreateTask(context.Background(), orchestration.TaskSpec{
		TeamID: root.TeamID, Title: "Review before merge", VerificationProfile: "reviewer-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.TransitionTask(context.Background(), orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskReady}); err != nil {
		t.Fatal(err)
	}
	var worker orchestration.AgentSnapshot
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		agents, listErr := store.ListAgents(context.Background(), orchestration.AgentQuery{TeamID: root.TeamID, ParentAgentID: root.AgentID})
		if listErr != nil {
			t.Fatal(listErr)
		}
		for _, agent := range agents {
			if agent.AssignedTaskID == task.ID {
				worker = agent
				break
			}
		}
		if worker.ID != "" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if worker.ID == "" {
		t.Fatal("worker was not dispatched")
	}
	waited, err := supervisor.Wait(context.Background(), WaitCommand{AgentIDs: []orchestration.AgentID{worker.ID}, Timeout: time.Second})
	if err != nil || !waited.Satisfied || waited.Agents[0].State != orchestration.AgentCompleted {
		t.Fatalf("worker did not finish: result=%#v err=%v", waited, err)
	}
	verified, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if verified.State != orchestration.TaskVerification || verified.AssigneeAgentID != worker.ID {
		t.Fatalf("worker completion bypassed verification: %#v", verified)
	}
	agents, err := store.ListAgents(context.Background(), orchestration.AgentQuery{TeamID: root.TeamID, ParentAgentID: root.AgentID})
	if err != nil {
		t.Fatal(err)
	}
	var reviewer orchestration.AgentSnapshot
	for _, agent := range agents {
		if agent.ID != worker.ID && agent.ProfileRevision == "reviewer-v1" {
			reviewer = agent
			break
		}
	}
	if reviewer.ID == "" {
		t.Fatalf("reviewer child was not spawned: %#v", agents)
	}
}

func TestSupervisorConvertsInvalidRunnerResultToFailure(t *testing.T) {
	runner := invalidResultRunner{}
	supervisor, _, root := newSupervisorTest(t, runner, 4, 3, 4)
	child, err := supervisor.Spawn(context.Background(), childSpec(root, "invalid-result", "return a result"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := supervisor.Wait(context.Background(), WaitCommand{AgentIDs: []orchestration.AgentID{child.ID}, Timeout: time.Second})
	if err != nil || !result.Satisfied || result.Agents[0].State != orchestration.AgentFailed {
		t.Fatalf("invalid runner result was not failed: result=%#v err=%v", result, err)
	}
}

func TestSupervisorInterruptsRunningChild(t *testing.T) {
	runner := &testRunner{started: make(chan RunRequest, 1)}
	supervisor, store, root := newSupervisorTest(t, runner, 4, 3, 4)
	child, err := supervisor.Spawn(context.Background(), childSpec(root, "interrupt-running", "wait for cancellation"))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("child did not start")
	}
	if err := supervisor.Interrupt(context.Background(), child.ID); err != nil {
		t.Fatal(err)
	}
	waited, err := supervisor.Wait(context.Background(), WaitCommand{AgentIDs: []orchestration.AgentID{child.ID}, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if !waited.Satisfied || waited.Agents[0].State != orchestration.AgentCanceled {
		t.Fatalf("unexpected cancellation result: %#v", waited)
	}
	persisted, err := store.GetAgent(context.Background(), child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != orchestration.AgentCanceled {
		t.Fatalf("child was not durably canceled: %#v", persisted)
	}
}

func TestSupervisorInterruptCascadesToRunningDescendants(t *testing.T) {
	runner := &testRunner{started: make(chan RunRequest, 2)}
	supervisor, store, root := newSupervisorTestWithRunning(t, runner, 2, 4, 3, 4)
	child, err := supervisor.Spawn(context.Background(), childSpec(root, "interrupt-parent", "wait for cancellation"))
	if err != nil {
		t.Fatal(err)
	}
	grandchildSpec := childSpec(root, "interrupt-grandchild", "wait for cancellation")
	// A reader runs beside its writing parent.
	grandchildSpec.ParentAgentID, grandchildSpec.Workspace = child.ID, orchestration.WorkspaceReadonly
	grandchild, err := supervisor.Spawn(context.Background(), grandchildSpec)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case <-runner.started:
		case <-time.After(time.Second):
			t.Fatal("Agents did not start")
		}
	}
	if err := supervisor.Interrupt(context.Background(), child.ID); err != nil {
		t.Fatal(err)
	}
	waited, err := supervisor.Wait(context.Background(), WaitCommand{AgentIDs: []orchestration.AgentID{child.ID, grandchild.ID}, Timeout: time.Second})
	if err != nil || !waited.Satisfied {
		t.Fatalf("interrupted tree did not stop: %#v err=%v", waited, err)
	}
	for _, id := range []orchestration.AgentID{child.ID, grandchild.ID} {
		persisted, err := store.GetAgent(context.Background(), id)
		if err != nil || persisted.State != orchestration.AgentCanceled {
			t.Fatalf("Agent %s = %s, want canceled; err=%v", id, persisted.State, err)
		}
	}
}

func TestSupervisorInterruptsQueuedChild(t *testing.T) {
	supervisor, store, root := newSupervisorTest(t, nil, 4, 3, 4)
	child, err := supervisor.Spawn(context.Background(), childSpec(root, "interrupt-queued", "never run"))
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Interrupt(context.Background(), child.ID); err != nil {
		t.Fatal(err)
	}
	persisted, err := store.GetAgent(context.Background(), child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != orchestration.AgentCanceled {
		t.Fatalf("queued child was not canceled: %#v", persisted)
	}
}

func TestSupervisorWaitTimeoutAndAny(t *testing.T) {
	runner := &testRunner{started: make(chan RunRequest, 2)}
	supervisor, _, root := newSupervisorTest(t, runner, 4, 3, 4)
	first, err := supervisor.Spawn(context.Background(), childSpec(root, "wait-first", "first"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := supervisor.Spawn(context.Background(), childSpec(root, "wait-second", "second"))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("children did not start")
	}
	result, err := supervisor.Wait(context.Background(), WaitCommand{AgentIDs: []orchestration.AgentID{first.ID, second.ID}, Timeout: 10 * time.Millisecond})
	if err != nil || !result.TimedOut || result.Satisfied {
		t.Fatalf("unexpected timeout: result=%#v err=%v", result, err)
	}
	if err := supervisor.Interrupt(context.Background(), first.ID); err != nil {
		t.Fatal(err)
	}
	result, err = supervisor.Wait(context.Background(), WaitCommand{AgentIDs: []orchestration.AgentID{first.ID, second.ID}, Any: true, Timeout: time.Second})
	if err != nil || !result.Satisfied {
		t.Fatalf("any wait was not satisfied: result=%#v err=%v", result, err)
	}
	_ = supervisor.Interrupt(context.Background(), second.ID)
}

func TestSupervisorRunsReadonlyChildrenInParallel(t *testing.T) {
	runner := &testRunner{started: make(chan RunRequest, 4)}
	supervisor, _, root := newSupervisorTestWithRunning(t, runner, 4, 8, 3, 8)
	children := make([]orchestration.AgentID, 0, 4)
	for index := range 4 {
		spec := childSpec(root, fmt.Sprintf("readonly-%d", index), "inspect")
		spec.Workspace = orchestration.WorkspaceReadonly
		child, err := supervisor.Spawn(context.Background(), spec)
		if err != nil {
			t.Fatal(err)
		}
		children = append(children, child.ID)
	}
	// Each run blocks until it is interrupted, so four starts mean the four
	// executions overlap.
	for range 4 {
		select {
		case <-runner.started:
		case <-time.After(time.Second):
			t.Fatal("read-only children did not all run at the same time")
		}
	}
	for _, child := range children {
		if err := supervisor.Interrupt(context.Background(), child); err != nil {
			t.Fatal(err)
		}
	}
	result, err := supervisor.Wait(context.Background(), WaitCommand{AgentIDs: children, Timeout: time.Second})
	if err != nil || !result.Satisfied {
		t.Fatalf("parallel children did not finish: result=%#v err=%v", result, err)
	}
}

func TestSupervisorSerializesSharedWorkspaceWriters(t *testing.T) {
	runner := &testRunner{started: make(chan RunRequest, 3)}
	supervisor, _, root := newSupervisorTestWithRunning(t, runner, 4, 8, 3, 8)
	firstWriter, err := supervisor.Spawn(context.Background(), childSpec(root, "writer-first", "edit"))
	if err != nil {
		t.Fatal(err)
	}
	secondWriter, err := supervisor.Spawn(context.Background(), childSpec(root, "writer-second", "edit"))
	if err != nil {
		t.Fatal(err)
	}
	readerSpec := childSpec(root, "reader", "inspect")
	readerSpec.Workspace = orchestration.WorkspaceReadonly
	reader, err := supervisor.Spawn(context.Background(), readerSpec)
	if err != nil {
		t.Fatal(err)
	}
	running := map[orchestration.AgentID]bool{}
	for range 2 {
		select {
		case request := <-runner.started:
			running[request.Agent.ID] = true
		case <-time.After(time.Second):
			t.Fatalf("expected one writer and the reader to start, got %v", running)
		}
	}
	if !running[firstWriter.ID] || !running[reader.ID] {
		t.Fatalf("started children = %v, want the first writer and the reader", running)
	}
	select {
	case request := <-runner.started:
		t.Fatalf("second writer %s started while the first was running", request.Agent.ID)
	case <-time.After(200 * time.Millisecond):
	}
	if err := supervisor.Interrupt(context.Background(), firstWriter.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-runner.started:
		if request.Agent.ID != secondWriter.ID {
			t.Fatalf("started %s after the first writer stopped, want the second writer", request.Agent.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("second writer did not start after the first writer finished")
	}
	for _, child := range []orchestration.AgentID{secondWriter.ID, reader.ID} {
		if err := supervisor.Interrupt(context.Background(), child); err != nil {
			t.Fatal(err)
		}
	}
	result, err := supervisor.Wait(context.Background(), WaitCommand{AgentIDs: []orchestration.AgentID{firstWriter.ID, secondWriter.ID, reader.ID}, Timeout: time.Second})
	if err != nil || !result.Satisfied {
		t.Fatalf("children did not finish: result=%#v err=%v", result, err)
	}
}

func TestSupervisorReleasesWorkspaceViewAfterTerminalRun(t *testing.T) {
	runner := &testRunner{result: orchestration.AgentResult{Status: orchestration.ResultSucceeded, Summary: "done"}}
	supervisor, store, root := newSupervisorTest(t, runner, 4, 3, 4)
	coordinator, err := workspacecoord.NewCoordinator(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	supervisor.workspace = coordinator
	supervisor.workspaceRoot = t.TempDir()
	child, err := supervisor.Spawn(context.Background(), childSpec(root, "workspace-view-terminal", "run"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := supervisor.Wait(context.Background(), WaitCommand{AgentIDs: []orchestration.AgentID{child.ID}, Timeout: time.Second})
	if err != nil || !result.Satisfied {
		t.Fatalf("child did not finish: result=%#v err=%v", result, err)
	}
	views, err := store.ListViews(context.Background(), workspacecoord.ViewQuery{TeamID: root.TeamID, AgentID: child.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].State != workspacecoord.ViewReleased {
		t.Fatalf("terminal Agent view = %#v", views)
	}
}

func TestSupervisorCreatesViewFromGrantedWorkspaceMode(t *testing.T) {
	runner := &testRunner{result: orchestration.AgentResult{Status: orchestration.ResultSucceeded, Summary: "done"}}
	supervisor, store, root := newSupervisorTest(t, runner, 4, 3, 4)
	coordinator, err := workspacecoord.NewCoordinator(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	supervisor.workspace = coordinator
	supervisor.workspaceRoot = t.TempDir()
	spec := childSpec(root, "readonly-view", "inspect")
	spec.Workspace = orchestration.WorkspaceReadonly
	child, err := supervisor.Spawn(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	result, err := supervisor.Wait(context.Background(), WaitCommand{AgentIDs: []orchestration.AgentID{child.ID}, Timeout: time.Second})
	if err != nil || !result.Satisfied {
		t.Fatalf("child did not finish: result=%#v err=%v", result, err)
	}
	views, err := store.ListViews(context.Background(), workspacecoord.ViewQuery{TeamID: root.TeamID, AgentID: child.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Mode != workspacecoord.ViewSharedReadonly {
		t.Fatalf("read-only child view = %#v", views)
	}
}

func TestSupervisorReleasesActiveLeasesAfterTerminalRun(t *testing.T) {
	runner := &testRunner{started: make(chan RunRequest, 1)}
	supervisor, _, root := newSupervisorTest(t, runner, 4, 3, 4)
	child, err := supervisor.Spawn(context.Background(), childSpec(root, "lease-terminal", "run"))
	if err != nil {
		t.Fatal(err)
	}
	request := <-runner.started
	lease, err := supervisor.leases.AcquireLease(context.Background(), orchestration.LeaseSpec{
		TeamID: root.TeamID, AgentID: child.ID, RunID: request.Agent.CurrentRunID, Path: "src/file.go", Mode: orchestration.LeaseWrite,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Interrupt(context.Background(), child.ID); err != nil {
		t.Fatal(err)
	}
	result, err := supervisor.Wait(context.Background(), WaitCommand{AgentIDs: []orchestration.AgentID{child.ID}, Timeout: time.Second})
	if err != nil || !result.Satisfied {
		t.Fatalf("canceled child did not finish: result=%#v err=%v", result, err)
	}
	leases, err := supervisor.leases.ListLeases(context.Background(), orchestration.LeaseQuery{TeamID: root.TeamID, AgentID: child.ID, States: []orchestration.LeaseState{orchestration.LeaseActive}})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 0 {
		t.Fatalf("active leases after terminal run = %#v (created %s)", leases, lease.ID)
	}
}

func TestSupervisorEnforcesActiveLimitsAndIdempotency(t *testing.T) {
	runner := &testRunner{started: make(chan RunRequest, 2)}
	supervisor, _, root := newSupervisorTest(t, runner, 2, 3, 1)
	first, err := supervisor.Spawn(context.Background(), childSpec(root, "limited", "first"))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("first child did not start")
	}
	duplicate, err := supervisor.Spawn(context.Background(), childSpec(root, "limited", "first"))
	if err != nil || duplicate.ID != first.ID {
		t.Fatalf("idempotent retry failed: duplicate=%#v err=%v", duplicate, err)
	}
	if _, err := supervisor.Spawn(context.Background(), childSpec(root, "limited", "different prompt")); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("got %v, want idempotency conflict", err)
	}
	if _, err := supervisor.Spawn(context.Background(), childSpec(root, "second", "at capacity")); !errors.Is(err, orchestration.ErrAgentLimit) {
		t.Fatalf("got %v, want live-agent limit", err)
	}
	if err := supervisor.Interrupt(context.Background(), first.ID); err != nil {
		t.Fatal(err)
	}
	waited, err := supervisor.Wait(context.Background(), WaitCommand{AgentIDs: []orchestration.AgentID{first.ID}, Timeout: time.Second})
	if err != nil || !waited.Satisfied {
		t.Fatalf("first child did not finish: %#v %v", waited, err)
	}
	second, err := supervisor.Spawn(context.Background(), childSpec(root, "second", "after completion"))
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Fatal("new spawn reused a completed child")
	}
	_ = supervisor.Interrupt(context.Background(), second.ID)
}

func TestControlToolBindingsUseActorScopeAndToolCallId(t *testing.T) {
	supervisor, _, root := newSupervisorTest(t, nil, 4, 3, 4)
	bindingSet := Bindings(supervisor)
	if len(bindingSet) != 7 {
		t.Fatalf("got %d control tools, want 7", len(bindingSet))
	}
	for _, binding := range bindingSet {
		if err := binding.Descriptor.Validate(); err != nil {
			t.Fatalf("invalid control tool %q: %v", binding.Descriptor.Name, err)
		}
	}
	ctx := orchestration.WithToolCallID(orchestration.WithActorScope(context.Background(), root.ActorScope()), "tool-call-1")
	var spawnBinding, messageBinding appTool.Binding
	for _, binding := range bindingSet {
		if binding.Descriptor.Name == "agent_spawn" {
			spawnBinding = binding
		}
		if binding.Descriptor.Name == "agent_message" {
			messageBinding = binding
		}
	}
	data, err := spawnBinding.Executor.Invoke(ctx, json.RawMessage(`{"prompt":"child task"}`))
	if err != nil {
		t.Fatal(err)
	}
	var spawned struct {
		AgentID orchestration.AgentID `json:"agent_id"`
	}
	if err := json.Unmarshal(data, &spawned); err != nil || spawned.AgentID == "" {
		t.Fatalf("invalid spawn result: %s (%v)", data, err)
	}
	second, err := spawnBinding.Executor.Invoke(ctx, json.RawMessage(`{"prompt":"child task"}`))
	if err != nil {
		t.Fatal(err)
	}
	var repeated struct {
		AgentID orchestration.AgentID `json:"agent_id"`
	}
	if err := json.Unmarshal(second, &repeated); err != nil || repeated.AgentID != spawned.AgentID {
		t.Fatalf("tool-call id did not make spawn idempotent: %s", second)
	}
	messageData, err := messageBinding.Executor.Invoke(ctx, json.RawMessage(`{"recipient_agent_id":"`+string(spawned.AgentID)+`","type":"status","body":{"progress":"started"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(messageData) {
		t.Fatalf("invalid message result: %s", messageData)
	}
}

func TestTaskControlToolBindingsUseActorScope(t *testing.T) {
	supervisor, store, root := newSupervisorTest(t, nil, 4, 3, 4)
	supervisor.tasks = store
	bindings := Bindings(supervisor)
	if len(bindings) != 18 {
		t.Fatalf("got %d control tools, want 18", len(bindings))
	}
	byName := make(map[string]appTool.Binding, len(bindings))
	for _, binding := range bindings {
		byName[binding.Descriptor.Name] = binding
	}
	ctx := orchestration.WithToolCallID(orchestration.WithActorScope(context.Background(), root.ActorScope()), "task-create-call")
	data, err := byName["task_create"].Executor.Invoke(ctx, json.RawMessage(`{"title":"implement feature","description":"write the change"}`))
	if err != nil {
		t.Fatal(err)
	}
	var created orchestration.TaskSnapshot
	if err := json.Unmarshal(data, &created); err != nil || created.ID == "" {
		t.Fatalf("invalid task create result: %s (%v)", data, err)
	}
	repeated, err := byName["task_create"].Executor.Invoke(ctx, json.RawMessage(`{"title":"implement feature","description":"write the change"}`))
	if err != nil {
		t.Fatal(err)
	}
	var repeatedTask orchestration.TaskSnapshot
	if err := json.Unmarshal(repeated, &repeatedTask); err != nil || repeatedTask.ID != created.ID {
		t.Fatalf("task-call id did not make create idempotent: %s", repeated)
	}
	ready, err := byName["task_transition"].Executor.Invoke(ctx, json.RawMessage(`{"task_id":"`+string(created.ID)+`","target":"ready","expected_version":1}`))
	if err != nil {
		t.Fatal(err)
	}
	var readyTask orchestration.TaskSnapshot
	if err := json.Unmarshal(ready, &readyTask); err != nil || readyTask.State != orchestration.TaskReady {
		t.Fatalf("invalid task transition result: %s (%v)", ready, err)
	}
	claimed, err := byName["task_claim"].Executor.Invoke(ctx, json.RawMessage(`{"task_id":"`+string(created.ID)+`","expected_version":2}`))
	if err != nil {
		t.Fatal(err)
	}
	var claimedTask orchestration.TaskSnapshot
	if err := json.Unmarshal(claimed, &claimedTask); err != nil || claimedTask.State != orchestration.TaskClaimed || claimedTask.AssigneeAgentID != root.AgentID {
		t.Fatalf("invalid task claim result: %s (%v)", claimed, err)
	}
	listed, err := byName["task_list"].Executor.Invoke(ctx, json.RawMessage(`{"states":["claimed"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var tasks []orchestration.TaskSnapshot
	if err := json.Unmarshal(listed, &tasks); err != nil || len(tasks) != 1 || tasks[0].ID != created.ID {
		t.Fatalf("invalid task list result: %s (%v)", listed, err)
	}
}

func TestSupervisorRecoversDurableRunningChild(t *testing.T) {
	database, err := testfixture.Open(t, context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: "recovery-root"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.CreateChild(context.Background(), childSpec(root, "recovery-child", "resume this task"))
	if err != nil {
		t.Fatal(err)
	}
	runID, err := orchestration.NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionAgent(context.Background(), orchestration.AgentTransition{
		AgentID: child.ID, Expected: child.Version, Target: orchestration.AgentRunning, RunID: runID, Activity: "model",
	}); err != nil {
		t.Fatal(err)
	}
	runner := &testRunner{result: orchestration.AgentResult{Status: orchestration.ResultSucceeded, Summary: "recovered"}}
	supervisor, err := NewSupervisor(Config{Agents: store, Runner: runner, MaxRunningAgents: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Close() })
	if err := supervisor.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := supervisor.Wait(context.Background(), WaitCommand{AgentIDs: []orchestration.AgentID{child.ID}, Timeout: time.Second})
	if err != nil || !result.Satisfied || result.Agents[0].State != orchestration.AgentCompleted {
		t.Fatalf("recovery did not complete child: result=%#v err=%v", result, err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.requests) != 1 || !runner.requests[0].Recover || runner.requests[0].Agent.CurrentRunID != runID || runner.requests[0].Prompt != "" {
		t.Fatalf("recovery replaced durable run: %#v", runner.requests)
	}
}

func TestSupervisorPersistsSeededContextAtSpawn(t *testing.T) {
	database, err := testfixture.Open(t, context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: "seed-root"})
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := NewSupervisor(Config{
		Agents: store,
		ContextSeeder: ContextSeederFunc(func(context.Context, orchestration.AgentSnapshot, string) (string, error) {
			return "parent context", nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Close() })
	child, err := supervisor.Spawn(context.Background(), childSpec(root, "seeded-context", "child task"))
	if err != nil {
		t.Fatal(err)
	}
	if child.InitialPrompt != "parent context\n\nchild task" || child.ContextPolicy != "summary" {
		t.Fatalf("context seed was not persisted: %#v", child)
	}
	persisted, err := store.GetAgent(context.Background(), child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.InitialPrompt != child.InitialPrompt {
		t.Fatalf("persisted prompt differs: %#v", persisted)
	}
}

func TestSupervisorReconcilesDurableWaitSet(t *testing.T) {
	database, err := testfixture.Open(t, context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: "supervisor-wait-root"})
	if err != nil {
		t.Fatal(err)
	}
	waiter, err := store.CreateChild(context.Background(), childSpec(root, "waiter", "wait"))
	if err != nil {
		t.Fatal(err)
	}
	target, err := store.CreateChild(context.Background(), childSpec(root, "wait-target", "target"))
	if err != nil {
		t.Fatal(err)
	}
	runID, err := orchestration.NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	waiter, err = store.TransitionAgent(context.Background(), orchestration.AgentTransition{AgentID: waiter.ID, Expected: waiter.Version, Target: orchestration.AgentRunning, RunID: runID, Activity: "runner"})
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := NewSupervisor(Config{Agents: store, WaitSets: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Close() })
	waitSet, err := supervisor.Suspend(context.Background(), orchestration.WaitSpec{
		TeamID: root.TeamID, AgentID: waiter.ID, RunID: runID, Condition: "children", TargetAgentIDs: []orchestration.AgentID{target.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if waitSet.State != orchestration.WaitSetPending {
		t.Fatalf("unexpected wait set state: %#v", waitSet)
	}
	completed, err := store.TransitionAgent(context.Background(), orchestration.AgentTransition{AgentID: target.ID, Expected: target.Version, Target: orchestration.AgentRunning, RunID: runID, Activity: "runner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionAgent(context.Background(), orchestration.AgentTransition{AgentID: completed.ID, Expected: completed.Version, Target: orchestration.AgentCompleted, RunID: runID, Activity: "succeeded", Result: &orchestration.AgentResult{Status: orchestration.ResultSucceeded, Summary: "done"}}); err != nil {
		t.Fatal(err)
	}
	count, err := supervisor.ReconcileWaitSets(context.Background(), root.TeamID)
	if err != nil || count != 1 {
		t.Fatalf("wait set was not reconciled: count=%d err=%v", count, err)
	}
	resumed, err := store.GetAgent(context.Background(), waiter.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.State != orchestration.AgentQueued {
		t.Fatalf("waiter was not requeued: %#v", resumed)
	}
}

func TestAgentWaitToolCreatesDurableSuspension(t *testing.T) {
	database, err := testfixture.Open(t, context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: "wait-tool-root"})
	if err != nil {
		t.Fatal(err)
	}
	waiter, err := store.CreateChild(context.Background(), childSpec(root, "wait-tool-waiter", "wait"))
	if err != nil {
		t.Fatal(err)
	}
	target, err := store.CreateChild(context.Background(), orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: waiter.ID, ProfileRevision: "worker-v1", IdempotencyKey: "wait-tool-target", InitialPrompt: "target"})
	if err != nil {
		t.Fatal(err)
	}
	runID, err := orchestration.NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionAgent(context.Background(), orchestration.AgentTransition{AgentID: waiter.ID, Expected: waiter.Version, Target: orchestration.AgentRunning, RunID: runID, Activity: "runner"}); err != nil {
		t.Fatal(err)
	}
	supervisor, err := NewSupervisor(Config{Agents: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Close() })
	bindings := Bindings(supervisor)
	var waitBinding appTool.Binding
	for _, binding := range bindings {
		if binding.Descriptor.Name == "agent_wait" {
			waitBinding = binding
			break
		}
	}
	ctx := orchestration.WithTurnID(orchestration.WithToolCallID(orchestration.WithActorScope(context.Background(), orchestration.AgentSnapshot{
		ID: waiter.ID, TeamID: root.TeamID, ThreadID: waiter.ThreadID, CurrentRunID: runID,
	}.ActorScope()), "wait-tool-call"), "turn-for-wait")
	if _, err := waitBinding.Executor.Invoke(ctx, json.RawMessage(`{"agent_ids":["`+string(target.ID)+`"]}`)); err == nil {
		t.Fatal("agent_wait unexpectedly returned success")
	} else {
		var suspended *WaitSuspendedError
		if !errors.As(err, &suspended) {
			t.Fatalf("wait tool error = %v, want WaitSuspendedError", err)
		}
		if suspended.WaitSet.ResumeToken != "turn-for-wait" {
			t.Fatalf("wait resume token = %q, want turn ID", suspended.WaitSet.ResumeToken)
		}
		// The real Eino adapter calls Suspend only after its checkpoint
		// interrupt has been durably stored.
		if _, err := supervisor.Suspend(context.Background(), suspended.WaitSpec()); err != nil {
			t.Fatalf("persist checkpointed wait: %v", err)
		}
	}
	persisted, err := store.GetAgent(context.Background(), waiter.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != orchestration.AgentWaiting {
		t.Fatalf("wait tool did not suspend Agent: %#v", persisted)
	}
}

func TestSupervisorResumesSuspendedRunnerWithOriginalToken(t *testing.T) {
	runner := &resumeRunner{started: make(chan RunRequest, 2)}
	supervisor, store, root := newSupervisorTest(t, runner, 4, 3, 4)
	target, err := store.CreateChild(context.Background(), childSpec(root, "resume-target", "target"))
	if err != nil {
		t.Fatal(err)
	}
	targetRunID, err := orchestration.NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	target, err = store.TransitionAgent(context.Background(), orchestration.AgentTransition{AgentID: target.ID, Expected: target.Version, Target: orchestration.AgentRunning, RunID: targetRunID, Activity: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionAgent(context.Background(), orchestration.AgentTransition{AgentID: target.ID, Expected: target.Version, Target: orchestration.AgentCompleted, RunID: targetRunID, Activity: "succeeded", Result: &orchestration.AgentResult{Status: orchestration.ResultSucceeded, Summary: "fixture"}}); err != nil {
		t.Fatal(err)
	}
	runner.target = target.ID
	child, err := supervisor.Spawn(context.Background(), childSpec(root, "resume-waiter", "wait then resume"))
	if err != nil {
		t.Fatal(err)
	}
	var originalRunID orchestration.RunID
	select {
	case initial := <-runner.started:
		originalRunID = initial.Agent.CurrentRunID
	case <-time.After(time.Second):
		t.Fatal("suspending runner did not start")
	}
	var waitSet orchestration.WaitSet
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		waitSets, listErr := store.ListWaitSets(context.Background(), orchestration.WaitSetQuery{TeamID: root.TeamID, AgentID: child.ID, States: []orchestration.WaitSetState{orchestration.WaitSetPending}})
		if listErr != nil {
			t.Fatal(listErr)
		}
		if len(waitSets) == 1 {
			waitSet = waitSets[0]
			break
		}
		time.Sleep(time.Millisecond)
	}
	if waitSet.ID == "" {
		state, _ := store.GetAgent(context.Background(), child.ID)
		allWaits, _ := store.ListWaitSets(context.Background(), orchestration.WaitSetQuery{TeamID: root.TeamID})
		runner.mu.Lock()
		requests := append([]RunRequest(nil), runner.requests...)
		runner.mu.Unlock()
		t.Fatalf("suspension was not persisted: agent=%#v waits=%#v requests=%#v", state, allWaits, requests)
	}
	if _, err := supervisor.ResolveWaitSet(context.Background(), waitSet.ID, "test resume"); err != nil {
		t.Fatal(err)
	}
	select {
	case resumed := <-runner.started:
		if resumed.ResumeWaitSet != waitSet.ID || resumed.ResumeToken != "checkpoint-turn-1" || resumed.Agent.CurrentRunID != originalRunID {
			t.Fatalf("resume request = %#v", resumed)
		}
	case <-time.After(time.Second):
		t.Fatal("resumed runner did not start")
	}
	result, err := supervisor.Wait(context.Background(), WaitCommand{AgentIDs: []orchestration.AgentID{child.ID}, Timeout: time.Second})
	if err != nil || !result.Satisfied {
		t.Fatalf("resumed child did not finish: result=%#v err=%v", result, err)
	}
	consumed, err := store.GetWaitSet(context.Background(), waitSet.ID)
	if err != nil {
		t.Fatal(err)
	}
	if consumed.State != orchestration.WaitSetConsumed {
		t.Fatalf("wait set state = %s, want consumed", consumed.State)
	}
}

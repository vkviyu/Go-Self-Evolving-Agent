package atestore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

func TestTaskNativePlanAuthorizationIdempotencyAndVerification(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	team, owner := taskNativeSeed(t, store)
	spec := orchestration.TaskSpec{TeamID: team.ID, Title: "original", IdempotencyKey: "create", VerificationProfile: "review"}
	task, err := store.CreateTask(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := store.CreateTask(ctx, spec)
	if err != nil || replay.ID != task.ID {
		t.Fatalf("create replay: %+v %v", replay, err)
	}
	spec.Title = "different"
	if _, err := store.CreateTask(ctx, spec); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("create conflict: %v", err)
	}
	siblingID, _ := orchestration.NewAgentID()
	sibling := orchestration.AgentSnapshot{ID: siblingID, TeamID: team.ID, State: orchestration.AgentRunning, Version: 1}
	if err := store.authority.Update(ctx, func(tx *ateauthority.Tx) error { return createRecord(tx, "agent", string(sibling.ID), sibling) }); err != nil {
		t.Fatal(err)
	}
	title := " edited "
	update := orchestration.TaskUpdate{TaskID: task.ID, Expected: task.Version, ActorAgentID: sibling.ID, Title: &title, IdempotencyKey: "edit"}
	if _, err := store.UpdateTask(ctx, update); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("sibling edited: %v", err)
	}
	update.ActorAgentID = owner.ID
	task, err = store.UpdateTask(ctx, update)
	if err != nil || task.Title != "edited" {
		t.Fatalf("edit=%+v %v", task, err)
	}
	replay, err = store.UpdateTask(ctx, update)
	if err != nil || replay.Version != task.Version {
		t.Fatalf("edit replay=%+v %v", replay, err)
	}
	task = taskNativeTransition(t, store, task, orchestration.TaskReady, owner.ID)
	task, err = store.ClaimTask(ctx, task.ID, owner.ID, task.Version)
	if err != nil {
		t.Fatal(err)
	}
	task = taskNativeTransition(t, store, task, orchestration.TaskInProgress, owner.ID)
	if _, err := store.TransitionTask(ctx, orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskCompleted, ActorAgentID: owner.ID}); !errors.Is(err, orchestration.ErrTaskVerificationRequired) {
		t.Fatalf("skipped verification: %v", err)
	}
	task = taskNativeTransition(t, store, task, orchestration.TaskVerification, owner.ID)
	if _, err := store.TransitionTask(ctx, orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskCompleted, ActorAgentID: owner.ID}); !errors.Is(err, orchestration.ErrTaskReviewerRequired) {
		t.Fatalf("self review: %v", err)
	}
	taskNativeTransition(t, store, task, orchestration.TaskCompleted, sibling.ID)
}

func TestTaskNativeReaperAndRelease(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	team, owner := taskNativeSeed(t, store)
	task := taskNativeTransition(t, store, taskNativeCreate(t, store, team.ID, "claim"), orchestration.TaskReady, owner.ID)
	claimed, err := store.ClaimTask(ctx, task.ID, owner.ID, task.Version)
	if err != nil {
		t.Fatal(err)
	}
	owner.AssignedTaskID = task.ID
	if err := store.authority.Update(ctx, func(tx *ateauthority.Tx) error { return putRecord(tx, "agent", string(owner.ID), owner) }); err != nil {
		t.Fatal(err)
	}
	now := claimed.ClaimExpiresAt.Add(time.Second)
	if count, err := store.ReapExpiredTaskClaims(ctx, now); err != nil || count != 0 {
		t.Fatalf("stole live claim: %d %v", count, err)
	}
	owner.State = orchestration.AgentFailed
	if err := store.authority.Update(ctx, func(tx *ateauthority.Tx) error { return putRecord(tx, "agent", string(owner.ID), owner) }); err != nil {
		t.Fatal(err)
	}
	if count, err := store.ReapExpiredTaskClaims(ctx, now); err != nil || count != 1 {
		t.Fatalf("reap: %d %v", count, err)
	}
	task, err = store.GetTask(ctx, task.ID)
	if err != nil || task.Attempt != 1 || task.State != orchestration.TaskReady || task.ClaimExpiresAt != nil || task.AssigneeAgentID != "" {
		t.Fatalf("reaped=%+v %v", task, err)
	}
	task, err = store.ClaimTask(ctx, task.ID, owner.ID, task.Version)
	if err != nil {
		t.Fatal(err)
	}
	released, err := store.ReleaseTask(ctx, task.ID, owner.ID, task.Version)
	if err != nil || released.Attempt != 2 || released.State != orchestration.TaskReady {
		t.Fatalf("release=%+v %v", released, err)
	}
}

func taskProofEvents(risk string) []session.Event {
	start := session.Event{Type: session.EventTurnStarted, CorrelationID: "turn", Payload: session.MustJSONPayload(map[string]any{"turn_id": "turn", "run_id": "run"})}
	result := []session.Event{start}
	for _, kind := range []session.EventType{session.EventToolProposed, session.EventToolStarted, session.EventToolSucceeded} {
		result = append(result, session.Event{Type: kind, CorrelationID: "call", Payload: session.MustJSONPayload(map[string]any{"turn_id": "turn", "call_id": "call", "tool": "inspect", "revision": "v1", "risk_class": risk, "result": map[string]any{"ok": true}})})
	}
	return append(result, session.Event{Type: session.EventTurnFailed, CorrelationID: "turn", Payload: session.MustJSONPayload(map[string]any{})})
}

func TestTaskExecutionProof(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []session.Event
		run    orchestration.RunID
		safe   bool
	}{
		{"read", taskProofEvents("read"), "run", true},
		{"pure", taskProofEvents("pure"), "run", true},
		{"write", taskProofEvents("write"), "run", false},
		{"wrong run", taskProofEvents("read"), "another", false},
		{"missing success", append(taskProofEvents("read")[:3], taskProofEvents("read")[4]), "run", false},
		{"missing terminal", taskProofEvents("read")[:4], "run", false},
		{"unknown outcome", append(taskProofEvents("read"), session.Event{Type: session.EventToolOutcomeUnknown}), "run", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proof := taskNewReadOnlyProof(tc.run, true)
			var err error
			for _, event := range tc.events {
				if err = proof.consume(event); err != nil {
					break
				}
			}
			if err == nil {
				err = proof.finish()
			}
			if tc.safe && err != nil || !tc.safe && !errors.Is(err, orchestration.ErrTaskReassignUnsafe) {
				t.Fatalf("safe=%v err=%v", tc.safe, err)
			}
		})
	}
}

func TestTaskPlanCycleGraph(t *testing.T) {
	edges := []taskEdge{{TaskID: "parent", DependencyID: "child"}, {TaskID: "other", DependencyID: "parent"}}
	if !taskWouldCycle(edges, "child", "other") || !taskWouldCycle(nil, "self", "self") || taskWouldCycle(edges, "other", "child") {
		t.Fatal("combined graph cycle semantics differ")
	}
}

// This test provider participates in the same authority-root CAS. Production
// session.EventStore does not currently offer this optional capability.
type taskTestSessions struct{ authority *ateauthority.Store }

func (s *taskTestSessions) Append(ctx context.Context, id session.ID, expected uint64, pending ...session.PendingEvent) ([]session.Event, error) {
	var result []session.Event
	err := s.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		result = nil
		var sequence uint64
		if _, err := tx.Get(recordKey("task-test-session-sequence", string(id)), &sequence); err != nil {
			return err
		}
		if expected != session.AnySequence && sequence != expected {
			return session.ErrSequenceConflict
		}
		for _, item := range pending {
			sequence++
			event := session.Event{SessionID: id, Sequence: sequence, Type: item.Type, CorrelationID: item.CorrelationID, Payload: item.Payload}
			if err := createRecord(tx, "task-test-session-event", taskPair(string(id), fmt.Sprint(sequence)), event); err != nil {
				return err
			}
			result = append(result, event)
		}
		return putRecord(tx, "task-test-session-sequence", string(id), sequence)
	})
	return result, err
}

func (s *taskTestSessions) CurrentSequence(ctx context.Context, id session.ID) (uint64, error) {
	var sequence uint64
	err := s.authority.View(ctx, func(tx *ateauthority.Tx) error {
		_, err := tx.Get(recordKey("task-test-session-sequence", string(id)), &sequence)
		return err
	})
	return sequence, err
}

func (s *taskTestSessions) Load(ctx context.Context, id session.ID, after uint64, limit int) ([]session.Event, error) {
	var result []session.Event
	err := s.authority.View(ctx, func(tx *ateauthority.Tx) error {
		events, err := listRecords[session.Event](tx, "task-test-session-event")
		if err != nil {
			return err
		}
		for _, event := range events {
			if event.SessionID == id && event.Sequence > after {
				result = append(result, event)
			}
		}
		slices.SortFunc(result, func(a, b session.Event) int {
			if a.Sequence < b.Sequence {
				return -1
			}
			if a.Sequence > b.Sequence {
				return 1
			}
			return 0
		})
		if len(result) > limit {
			result = result[:limit]
		}
		return nil
	})
	return result, err
}

func (s *taskTestSessions) ValidateTaskSessionFence(tx *ateauthority.Tx, id session.ID, sequence uint64) error {
	current, err := getRecord[uint64](tx, "task-test-session-sequence", string(id))
	if err != nil {
		return err
	}
	if current != sequence {
		return session.ErrSequenceConflict
	}
	return nil
}

type taskTestRacingSessions struct {
	*taskTestSessions
	advance bool
}

func (s *taskTestRacingSessions) Load(ctx context.Context, id session.ID, after uint64, limit int) ([]session.Event, error) {
	events, err := s.taskTestSessions.Load(ctx, id, after, limit)
	if err == nil && s.advance {
		s.advance = false
		_, err = s.Append(ctx, id, session.AnySequence, session.PendingEvent{
			Type: session.EventModelPartial, Payload: session.MustJSONPayload(map[string]any{}),
		})
	}
	return events, err
}

func TestTaskNativeRetryFenceAndIdempotency(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	team, owner := taskNativeSeed(t, store)
	task := taskNativeCreate(t, store, team.ID, "failed")
	owner.AssignedTaskID, owner.SessionID, owner.CurrentRunID, owner.State = task.ID, "task-retry-session", "run", orchestration.AgentFailed
	owner.Result = &orchestration.AgentResult{Status: orchestration.ResultFailed, Retryable: true, RetryCode: orchestration.TaskAttemptIterationLimit}
	task.State, task.AssigneeAgentID = orchestration.TaskFailed, owner.ID
	if err := store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		if err := putRecord(tx, "agent", string(owner.ID), owner); err != nil {
			return err
		}
		if err := putRecord(tx, "task", string(task.ID), task); err != nil {
			return err
		}
		return taskEvent(tx, task, orchestration.PendingEvent{Type: orchestration.EventTaskUpdated, CorrelationID: "run", Payload: orchestration.MustJSONPayload(map[string]any{"to": orchestration.TaskFailed, "assignee_agent_id": owner.ID, "attempt": 0})})
	}); err != nil {
		t.Fatal(err)
	}
	sessions := &taskTestSessions{authority: store.authority}
	for _, event := range taskProofEvents("read") {
		if _, err := sessions.Append(ctx, owner.SessionID, session.AnySequence, session.PendingEvent{Type: event.Type, CorrelationID: event.CorrelationID, Payload: event.Payload}); err != nil {
			t.Fatal(err)
		}
	}
	// Hide the optional capability while retaining authoritative history.
	store.sessions = struct{ session.EventStore }{sessions}
	if _, err := store.ReassignTask(ctx, task.ID, owner.ID, task.Version, "retry"); !errors.Is(err, orchestration.ErrTaskReassignUnsafe) || !strings.Contains(err.Error(), "cannot fence") {
		t.Fatalf("unfenced history accepted: %v", err)
	}
	unchanged, err := store.GetTask(ctx, task.ID)
	if err != nil || unchanged.Version != task.Version || unchanged.State != orchestration.TaskFailed {
		t.Fatalf("failed proof mutated task: %+v %v", unchanged, err)
	}
	store.sessions = &taskTestRacingSessions{taskTestSessions: sessions, advance: true}
	if _, err := store.ReassignTask(ctx, task.ID, owner.ID, task.Version, "racing retry"); !errors.Is(err, orchestration.ErrTaskReassignUnsafe) || !errors.Is(err, session.ErrSequenceConflict) {
		t.Fatalf("history advanced between proof and commit: %v", err)
	}
	store.sessions = sessions
	ready, err := store.ReassignTask(ctx, task.ID, owner.ID, task.Version, "retry")
	if err != nil || ready.State != orchestration.TaskReady || ready.Attempt != 1 || ready.AssigneeAgentID != "" {
		t.Fatalf("safe retry=%+v %v", ready, err)
	}
	claimed, err := store.ClaimTask(ctx, task.ID, owner.ID, ready.Version)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := store.ReassignTask(ctx, task.ID, owner.ID, task.Version, "retry again")
	if err != nil || replay.Version != claimed.Version || replay.State != orchestration.TaskClaimed || replay.Attempt != 1 {
		t.Fatalf("retry stole later claim: %+v %v", replay, err)
	}
}

func taskNativeSeed(t *testing.T, store *Store) (orchestration.TeamSnapshot, orchestration.AgentSnapshot) {
	t.Helper()
	teamID, err := orchestration.NewTeamID()
	if err != nil {
		t.Fatal(err)
	}
	agentID, err := orchestration.NewAgentID()
	if err != nil {
		t.Fatal(err)
	}
	team := orchestration.TeamSnapshot{ID: teamID, RootAgentID: agentID, State: orchestration.TeamActive, Version: 1}
	agent := orchestration.AgentSnapshot{ID: agentID, TeamID: teamID, State: orchestration.AgentRunning, Version: 1}
	err = store.authority.Update(context.Background(), func(tx *ateauthority.Tx) error {
		if err := createRecord(tx, "team", string(team.ID), team); err != nil {
			return err
		}
		return createRecord(tx, "agent", string(agent.ID), agent)
	})
	if err != nil {
		t.Fatal(err)
	}
	return team, agent
}

func taskNativeCreate(t *testing.T, store *Store, team orchestration.TeamID, title string) orchestration.TaskSnapshot {
	t.Helper()
	task, err := store.CreateTask(context.Background(), orchestration.TaskSpec{TeamID: team, Title: title})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func taskNativeTransition(t *testing.T, store *Store, task orchestration.TaskSnapshot, target orchestration.TaskState, actor orchestration.AgentID) orchestration.TaskSnapshot {
	t.Helper()
	updated, err := store.TransitionTask(context.Background(), orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: target, ActorAgentID: actor, AssigneeAgentID: task.AssigneeAgentID})
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func TestTaskNativeDependenciesClaimsAndVersions(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	team, owner := taskNativeSeed(t, store)
	a := taskNativeCreate(t, store, team.ID, "prerequisite")
	b := taskNativeCreate(t, store, team.ID, "dependent")
	if err := store.AddDependency(ctx, b.ID, a.ID, owner.ID, ""); err != nil {
		t.Fatal(err)
	}
	blocked, err := store.GetTask(ctx, b.ID)
	if err != nil || blocked.State != orchestration.TaskBlocked || blocked.Version != b.Version+1 {
		t.Fatalf("blocked=%+v err=%v", blocked, err)
	}
	if err := store.AddDependency(ctx, b.ID, a.ID, owner.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimTask(ctx, b.ID, owner.ID, blocked.Version); !errors.Is(err, orchestration.ErrTaskNotReady) {
		t.Fatalf("blocked claim: %v", err)
	}
	if err := store.AddDependency(ctx, a.ID, b.ID, owner.ID, ""); !errors.Is(err, orchestration.ErrTaskDependencyCycle) {
		t.Fatalf("cycle: %v", err)
	}
	a = taskNativeTransition(t, store, a, orchestration.TaskReady, owner.ID)
	claimed, err := store.ClaimTask(ctx, a.ID, owner.ID, a.Version)
	if err != nil || claimed.ClaimExpiresAt == nil {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}
	if _, err := store.ClaimTask(ctx, a.ID, owner.ID, a.Version); !errors.Is(err, orchestration.ErrTaskStateConflict) {
		t.Fatalf("stale claim: %v", err)
	}
	a = taskNativeTransition(t, store, claimed, orchestration.TaskInProgress, owner.ID)
	if a.ClaimExpiresAt != nil {
		t.Fatal("in-progress retains claim expiry")
	}
	taskNativeTransition(t, store, a, orchestration.TaskCompleted, owner.ID)
	ready, err := store.GetTask(ctx, b.ID)
	if err != nil || ready.State != orchestration.TaskReady || ready.Version != blocked.Version+1 {
		t.Fatalf("unblock=%+v err=%v", ready, err)
	}
	deps, err := store.ListDependencies(ctx, b.ID)
	if err != nil || !slices.Equal(deps, []orchestration.TaskID{a.ID}) {
		t.Fatalf("deps=%v err=%v", deps, err)
	}
}

func TestTaskNativeRefinementAtomicityGroupingAndReplay(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	team, owner := taskNativeSeed(t, store)
	parent := taskNativeCreate(t, store, team.ID, "parent")
	group, err := store.CreateTask(ctx, orchestration.TaskSpec{TeamID: team.ID, ParentTaskID: parent.ID, Title: "grouped only"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddDependency(ctx, group.ID, parent.ID, owner.ID, ""); err != nil {
		t.Fatal(err)
	}
	spec := orchestration.TaskRefinementSpec{TeamID: team.ID, ActorAgentID: owner.ID, ParentTaskID: parent.ID, Expected: parent.Version, IdempotencyKey: "refine", Children: []orchestration.TaskRefinementChild{{Key: "one", Title: "one"}, {Key: "two", Title: "two"}}, Dependencies: []orchestration.TaskRefinementDependency{{ChildKey: "two", DependencyChildKey: "one"}}}
	bad := spec
	bad.Dependencies = []orchestration.TaskRefinementDependency{{ChildKey: "one", DependencyTaskID: parent.ID}}
	if _, err := store.RefineTasks(ctx, bad); !errors.Is(err, orchestration.ErrTaskDependencyCycle) {
		t.Fatalf("refinement cycle: %v", err)
	}
	tasks, err := store.ListTasks(ctx, orchestration.TaskQuery{TeamID: team.ID})
	if err != nil || len(tasks) != 2 {
		t.Fatalf("partial refinement committed: %v %v", tasks, err)
	}
	refined, err := store.RefineTasks(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if refined.Children["two"].State != orchestration.TaskDraft {
		t.Fatal("refinement changed SQL draft semantics")
	}
	if err := store.AddDependency(ctx, refined.Children["one"].ID, parent.ID, owner.ID, ""); !errors.Is(err, orchestration.ErrTaskDependencyCycle) {
		t.Fatalf("membership cycle: %v", err)
	}
	parent, err = store.GetTask(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	parent = taskNativeTransition(t, store, parent, orchestration.TaskReady, owner.ID)
	parent, err = store.ClaimTask(ctx, parent.ID, owner.ID, parent.Version)
	if err != nil {
		t.Fatal(err)
	}
	parent = taskNativeTransition(t, store, parent, orchestration.TaskInProgress, owner.ID)
	for _, target := range []orchestration.TaskState{orchestration.TaskVerification, orchestration.TaskAwaitingMerge, orchestration.TaskCompleted} {
		if _, err := store.TransitionTask(ctx, orchestration.TaskTransition{TaskID: parent.ID, Expected: parent.Version, Target: target, ActorAgentID: owner.ID}); !errors.Is(err, orchestration.ErrTaskNotReady) {
			t.Fatalf("refinement gate %s: %v", target, err)
		}
	}
	replaySpec := spec
	replaySpec.Children = []orchestration.TaskRefinementChild{spec.Children[1], spec.Children[0]}
	replayed, err := store.RefineTasks(ctx, replaySpec)
	if err != nil || replayed.Children["one"].ID != refined.Children["one"].ID {
		t.Fatalf("replay=%+v err=%v", replayed, err)
	}
	for _, key := range []string{"one", "two"} {
		child := taskNativeTransition(t, store, refined.Children[key], orchestration.TaskReady, owner.ID)
		child, err = store.ClaimTask(ctx, child.ID, owner.ID, child.Version)
		if err != nil {
			t.Fatal(err)
		}
		child = taskNativeTransition(t, store, child, orchestration.TaskInProgress, owner.ID)
		taskNativeTransition(t, store, child, orchestration.TaskCompleted, owner.ID)
	}
	taskNativeTransition(t, store, parent, orchestration.TaskCompleted, owner.ID)
	group, err = store.GetTask(ctx, group.ID)
	if err != nil || group.State != orchestration.TaskReady {
		t.Fatalf("group blocked parent completion: %+v %v", group, err)
	}
}

func TestTaskNativeMergeGuardsAndDependencyRemoval(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	team, owner := taskNativeSeed(t, store)
	task := taskNativeCreate(t, store, team.ID, "merge")
	dependency := taskNativeCreate(t, store, team.ID, "dependency")
	if err := store.AddDependency(ctx, task.ID, dependency.ID, owner.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveDependency(ctx, task.ID, dependency.ID, owner.ID, ""); err != nil {
		t.Fatal(err)
	}
	task, err := store.GetTask(ctx, task.ID)
	if err != nil || task.State != orchestration.TaskReady {
		t.Fatalf("remove readiness: %+v %v", task, err)
	}
	version := task.Version
	if err := store.RemoveDependency(ctx, task.ID, dependency.ID, owner.ID, ""); err != nil {
		t.Fatal(err)
	}
	task, err = store.GetTask(ctx, task.ID)
	if err != nil || task.Version != version {
		t.Fatalf("absent removal changed version: %+v %v", task, err)
	}
	task, err = store.ClaimTask(ctx, task.ID, owner.ID, task.Version)
	if err != nil {
		t.Fatal(err)
	}
	task = taskNativeTransition(t, store, task, orchestration.TaskInProgress, owner.ID)
	transition := orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskAwaitingMerge, ActorAgentID: owner.ID}
	if _, err := store.TransitionTask(ctx, transition); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("await without merge: %v", err)
	}
	mergeID, _ := orchestration.NewMergeID()
	merge := taskWorkspaceRecord{TaskID: task.ID, AgentID: owner.ID, State: "pending"}
	if err := store.authority.Update(ctx, func(tx *ateauthority.Tx) error { return createRecord(tx, "merge", string(mergeID), merge) }); err != nil {
		t.Fatal(err)
	}
	transition.Target = orchestration.TaskCompleted
	if _, err := store.TransitionTask(ctx, transition); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("completed unmerged work: %v", err)
	}
	task = taskNativeTransition(t, store, task, orchestration.TaskAwaitingMerge, owner.ID)
	merge.State = "applied"
	if err := store.authority.Update(ctx, func(tx *ateauthority.Tx) error { return putRecord(tx, "merge", string(mergeID), merge) }); err != nil {
		t.Fatal(err)
	}
	taskNativeTransition(t, store, task, orchestration.TaskCompleted, owner.ID)
}

func TestTaskNativeClaimRaceAndAtomicSpawnHelper(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	team, owner := taskNativeSeed(t, store)
	task := taskNativeTransition(t, store, taskNativeCreate(t, store, team.ID, "race"), orchestration.TaskReady, owner.ID)
	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := store.ClaimTask(ctx, task.ID, owner.ID, task.Version)
			results <- err
		}()
	}
	success, conflict := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			success++
		} else if errors.Is(err, orchestration.ErrTaskStateConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("claim race: successes=%d conflicts=%d", success, conflict)
	}
	second := taskNativeTransition(t, store, taskNativeCreate(t, store, team.ID, "atomic helper"), orchestration.TaskReady, owner.ID)
	childID, _ := orchestration.NewAgentID()
	child := orchestration.AgentSnapshot{ID: childID, TeamID: team.ID, ParentAgentID: owner.ID, AssignedTaskID: second.ID, State: orchestration.AgentQueued, Version: 1}
	abort := errors.New("abort transaction after claim")
	err := store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		if err := createRecord(tx, "agent", string(child.ID), child); err != nil {
			return err
		}
		if _, err := claimTaskTx(tx, second.ID, child.ID, second.Version, time.Now().UTC()); err != nil {
			return err
		}
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatal(err)
	}
	current, err := store.GetTask(ctx, second.ID)
	if err != nil || current.State != orchestration.TaskReady || current.Version != second.Version {
		t.Fatalf("aborted helper changed task: %+v %v", current, err)
	}
	if err := store.authority.View(ctx, func(tx *ateauthority.Tx) error {
		_, err := getRecord[orchestration.AgentSnapshot](tx, "agent", string(child.ID))
		if !errors.Is(err, orchestration.ErrNotFound) {
			return fmt.Errorf("aborted helper retained agent: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

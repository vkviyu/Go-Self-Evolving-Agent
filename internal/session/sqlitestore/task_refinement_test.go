package sqlitestore

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

func refinementFixture(t *testing.T) (*Database, orchestration.RootThread, orchestration.TaskSnapshot) {
	t.Helper()
	database, err := Open(context.Background(), filepath.Join(t.TempDir(), "refinement.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	root, _, err := database.Orchestration().EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: "refinement"})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := database.Orchestration().CreateTask(context.Background(), orchestration.TaskSpec{TeamID: root.TeamID, Title: "Deliver feature"})
	if err != nil {
		t.Fatal(err)
	}
	return database, root, parent
}

func refinementSpec(root orchestration.RootThread, parent orchestration.TaskSnapshot) orchestration.TaskRefinementSpec {
	return orchestration.TaskRefinementSpec{TeamID: root.TeamID, ActorAgentID: root.AgentID, ParentTaskID: parent.ID, Expected: parent.Version,
		IdempotencyKey: "refine-1", CorrelationID: "call-1",
		Children:     []orchestration.TaskRefinementChild{{Key: "implement", Title: "Implement", Description: "Preserve public API", Priority: 2, RequiredProfile: "worker", VerificationProfile: "reviewer"}, {Key: "verify", Title: "Verify", RequiredProfile: "worker"}},
		Dependencies: []orchestration.TaskRefinementDependency{{ChildKey: "verify", DependencyChildKey: "implement"}},
	}
}

func TestTaskRefinementCommitsOnceAndReplaysAfterParentStops(t *testing.T) {
	database, root, parent := refinementFixture(t)
	store, ctx := database.Orchestration(), context.Background()
	spec := refinementSpec(root, parent)
	first, err := store.RefineTasks(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Children) != 2 || first.Children["implement"].ParentTaskID != parent.ID || first.Children["verify"].ParentTaskID != parent.ID {
		t.Fatalf("unexpected children: %+v", first)
	}
	updatedParent, err := store.GetTask(ctx, parent.ID)
	if err != nil || updatedParent.Version != parent.Version+1 || updatedParent.State != parent.State {
		t.Fatalf("parent version/state: %+v %v", updatedParent, err)
	}
	dependencies, err := store.ListDependencies(ctx, first.Children["verify"].ID)
	if err != nil || !slices.Equal(dependencies, []orchestration.TaskID{first.Children["implement"].ID}) {
		t.Fatalf("dependencies=%v err=%v", dependencies, err)
	}
	var before int
	if err := database.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM orchestration_events`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	// Child order and the transient correlation are not operation identity.
	spec.Children[0], spec.Children[1] = spec.Children[1], spec.Children[0]
	spec.CorrelationID = "retry-call"
	replayed, err := store.RefineTasks(ctx, spec)
	if err != nil || replayed.Children["implement"].ID != first.Children["implement"].ID || replayed.Children["verify"].ID != first.Children["verify"].ID {
		t.Fatalf("replay=%+v err=%v", replayed, err)
	}
	var after int
	if err := database.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM orchestration_events`).Scan(&after); err != nil || before != after {
		t.Fatalf("replay wrote events: %d -> %d (%v)", before, after, err)
	}
	if _, err := store.TransitionTask(ctx, orchestration.TaskTransition{TaskID: parent.ID, Expected: updatedParent.Version, Target: orchestration.TaskCanceled}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RefineTasks(ctx, spec); err != nil {
		t.Fatalf("committed refinement unavailable after parent stopped: %v", err)
	}
	spec.IdempotencyKey = "new-command"
	current, _ := store.GetTask(ctx, parent.ID)
	spec.Expected = current.Version
	if _, err := store.RefineTasks(ctx, spec); !errors.Is(err, orchestration.ErrTaskRefinementInvalid) {
		t.Fatalf("new refinement of stopped parent: %v", err)
	}
}

func TestTaskRefinementIdentityCoversEntirePlan(t *testing.T) {
	otherActor, err := orchestration.NewAgentID()
	if err != nil {
		t.Fatal(err)
	}
	otherTask, err := orchestration.NewTaskID()
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []struct {
		name   string
		change func(*orchestration.TaskRefinementSpec)
	}{
		{"child omitted", func(spec *orchestration.TaskRefinementSpec) {
			spec.Children = spec.Children[:1]
			spec.Dependencies = nil
		}},
		{"priority", func(spec *orchestration.TaskRefinementSpec) { spec.Children[0].Priority++ }},
		{"actor", func(spec *orchestration.TaskRefinementSpec) { spec.ActorAgentID = otherActor }},
		{"parent", func(spec *orchestration.TaskRefinementSpec) { spec.ParentTaskID = otherTask }},
		{"title", func(spec *orchestration.TaskRefinementSpec) { spec.Children[0].Title = "Changed title" }},
		{"description", func(spec *orchestration.TaskRefinementSpec) { spec.Children[0].Description += " changed" }},
		{"profile", func(spec *orchestration.TaskRefinementSpec) { spec.Children[0].RequiredProfile = "different" }},
		{"verification", func(spec *orchestration.TaskRefinementSpec) { spec.Children[0].VerificationProfile = "different" }},
		{"dependency omitted", func(spec *orchestration.TaskRefinementSpec) { spec.Dependencies = nil }},
		{"dependency changed", func(spec *orchestration.TaskRefinementSpec) {
			spec.Dependencies[0] = orchestration.TaskRefinementDependency{ChildKey: "verify", DependencyTaskID: otherTask}
		}},
		{"version", func(spec *orchestration.TaskRefinementSpec) { spec.Expected++ }},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			database, root, parent := refinementFixture(t)
			spec := refinementSpec(root, parent)
			if _, err := database.Orchestration().RefineTasks(context.Background(), spec); err != nil {
				t.Fatal(err)
			}
			mutation.change(&spec)
			if _, err := database.Orchestration().RefineTasks(context.Background(), spec); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
				t.Fatalf("changed command replayed: %v", err)
			}
		})
	}
}

func TestTaskRefinementCompetingCommandsUseParentVersion(t *testing.T) {
	database, root, parent := refinementFixture(t)
	start, errorsSeen := make(chan struct{}), make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for _, key := range []string{"first", "second"} {
		go func(key string) {
			spec := refinementSpec(root, parent)
			spec.IdempotencyKey = key
			ready.Done()
			<-start
			_, err := database.Orchestration().RefineTasks(context.Background(), spec)
			errorsSeen <- err
		}(key)
	}
	ready.Wait()
	close(start)
	var success, conflict int
	for range 2 {
		err := <-errorsSeen
		switch {
		case err == nil:
			success++
		case errors.Is(err, orchestration.ErrTaskStateConflict):
			conflict++
		default:
			t.Fatalf("unexpected competing command error: %v", err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflict)
	}
	tasks, err := database.Orchestration().ListTasks(context.Background(), orchestration.TaskQuery{TeamID: root.TeamID})
	if err != nil || len(tasks) != 3 {
		t.Fatalf("competing commands both committed: %+v %v", tasks, err)
	}
}

func TestTaskRefinementPreservesOrdinaryGroupingSemantics(t *testing.T) {
	database, root, group := refinementFixture(t)
	ctx, store := context.Background(), database.Orchestration()
	parent, err := store.CreateTask(ctx, orchestration.TaskSpec{TeamID: root.TeamID, ParentTaskID: group.ID, Title: "Grouped work"})
	if err != nil {
		t.Fatal(err)
	}
	spec := refinementSpec(root, parent)
	// Grouping does not mean that group must await parent. A new child may
	// depend on the earlier grouped preparation task without creating a cycle.
	spec.Dependencies = append(spec.Dependencies, orchestration.TaskRefinementDependency{ChildKey: "implement", DependencyTaskID: group.ID})
	if _, err := store.RefineTasks(ctx, spec); err != nil {
		t.Fatalf("ordinary grouping became an implicit dependency: %v", err)
	}
	var groupedMembers, refinedMembers int
	if err := database.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_refinement_members WHERE parent_task_id = ?`, string(group.ID)).Scan(&groupedMembers); err != nil {
		t.Fatal(err)
	}
	if err := database.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_refinement_members WHERE parent_task_id = ?`, string(parent.ID)).Scan(&refinedMembers); err != nil {
		t.Fatal(err)
	}
	if groupedMembers != 0 || refinedMembers != 2 {
		t.Fatalf("grouping/refinement relation mixed: %d/%d", groupedMembers, refinedMembers)
	}
}

func TestTaskRefinementRejectsOtherTeamActorAndDependency(t *testing.T) {
	database, root, parent := refinementFixture(t)
	ctx, store := context.Background(), database.Orchestration()
	otherRoot, _, err := store.EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "other-team"})
	if err != nil {
		t.Fatal(err)
	}
	spec := refinementSpec(root, parent)
	spec.ActorAgentID = otherRoot.AgentID
	if _, err := store.RefineTasks(ctx, spec); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("another Team's root refined this task: %v", err)
	}
	otherTask, err := store.CreateTask(ctx, orchestration.TaskSpec{TeamID: otherRoot.TeamID, Title: "Other Team work"})
	if err != nil {
		t.Fatal(err)
	}
	spec = refinementSpec(root, parent)
	spec.Dependencies = append(spec.Dependencies, orchestration.TaskRefinementDependency{ChildKey: "implement", DependencyTaskID: otherTask.ID})
	if _, err := store.RefineTasks(ctx, spec); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("cross-Team dependency accepted: %v", err)
	}
	tasks, err := store.ListTasks(ctx, orchestration.TaskQuery{TeamID: root.TeamID})
	if err != nil || len(tasks) != 1 || tasks[0].Version != parent.Version {
		t.Fatalf("cross-Team rejection left partial plan: %+v %v", tasks, err)
	}
}

func TestTaskRefinementAuthorizesCurrentOwnersAndTheirManagers(t *testing.T) {
	database, root, parent := refinementFixture(t)
	ctx, store := context.Background(), database.Orchestration()
	parent, err := store.TransitionTask(ctx, orchestration.TaskTransition{TaskID: parent.ID, Expected: parent.Version, Target: orchestration.TaskReady})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := store.CreateChild(ctx, orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "manager-v1", InitialPrompt: "Manage implementation"})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.CreateChild(ctx, orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: manager.ID, TaskID: parent.ID, ProfileRevision: "worker-v1", InitialPrompt: "Own implementation"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.CreateTask(ctx, orchestration.TaskSpec{TeamID: root.TeamID, ParentTaskID: parent.ID, Title: "Unassigned descendant"})
	if err != nil {
		t.Fatal(err)
	}
	spec := refinementSpec(root, child)
	spec.ActorAgentID = manager.ID
	if _, err := store.RefineTasks(ctx, spec); err != nil {
		t.Fatalf("manager of ancestor Task owner cannot refine descendant: %v", err)
	}
	child, err = store.GetTask(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	spec.ActorAgentID, spec.Expected, spec.IdempotencyKey = owner.ID, child.Version, "owner-refinement"
	if _, err := store.RefineTasks(ctx, spec); err != nil {
		t.Fatalf("ancestor Task owner cannot refine descendant: %v", err)
	}
	parent, err = store.GetTask(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReleaseTask(ctx, parent.ID, owner.ID, parent.Version); err != nil {
		t.Fatal(err)
	}
	child, err = store.GetTask(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	spec.Expected, spec.IdempotencyKey = child.Version, "after-release"
	if _, err := store.RefineTasks(ctx, spec); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("released run contract retained ancestor ownership: %v", err)
	}
}

func TestTaskRefinementReplayPrecedesTeamStateGate(t *testing.T) {
	database, root, parent := refinementFixture(t)
	ctx, store := context.Background(), database.Orchestration()
	spec := refinementSpec(root, parent)
	first, err := store.RefineTasks(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	team, err := store.GetTeam(ctx, root.TeamID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionTeam(ctx, orchestration.TeamTransition{TeamID: team.ID, Expected: team.Version, Target: orchestration.TeamCanceling}); err != nil {
		t.Fatal(err)
	}
	replayed, err := store.RefineTasks(ctx, spec)
	if err != nil || replayed.Children["implement"].ID != first.Children["implement"].ID {
		t.Fatalf("committed command unavailable during cancellation: %+v %v", replayed, err)
	}
	spec.IdempotencyKey = "new-after-cancel"
	if _, err := store.RefineTasks(ctx, spec); !errors.Is(err, orchestration.ErrTeamNotActive) {
		t.Fatalf("new command accepted after cancellation: %v", err)
	}
}

func refinementTaskTransition(t *testing.T, store *OrchestrationStore, task orchestration.TaskSnapshot, actor orchestration.AgentID, target orchestration.TaskState) orchestration.TaskSnapshot {
	t.Helper()
	transition := orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: target, ActorAgentID: actor}
	if target == orchestration.TaskClaimed {
		transition.AssigneeAgentID = actor
	}
	updated, err := store.TransitionTask(context.Background(), transition)
	if err != nil {
		t.Fatalf("transition %s to %s: %v", task.ID, target, err)
	}
	return updated
}

func TestTaskRefinementCompletionGateDoesNotChangeGrouping(t *testing.T) {
	for _, refined := range []bool{false, true} {
		name := "ordinary-grouping"
		if refined {
			name = "explicit-refinement"
		}
		t.Run(name, func(t *testing.T) {
			database, root, parent := refinementFixture(t)
			ctx, store := context.Background(), database.Orchestration()
			for _, target := range []orchestration.TaskState{orchestration.TaskReady, orchestration.TaskClaimed, orchestration.TaskInProgress} {
				parent = refinementTaskTransition(t, store, parent, root.AgentID, target)
			}
			if !refined {
				if _, err := store.CreateTask(ctx, orchestration.TaskSpec{TeamID: root.TeamID, ParentTaskID: parent.ID, Title: "Grouped follow-up"}); err != nil {
					t.Fatal(err)
				}
				parent = refinementTaskTransition(t, store, parent, root.AgentID, orchestration.TaskCompleted)
				return
			}
			spec := refinementSpec(root, parent)
			spec.Children[0].VerificationProfile = ""
			plan, err := store.RefineTasks(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			parent, err = store.GetTask(ctx, parent.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, target := range []orchestration.TaskState{orchestration.TaskVerification, orchestration.TaskAwaitingMerge, orchestration.TaskCompleted} {
				if _, err := store.TransitionTask(ctx, orchestration.TaskTransition{TaskID: parent.ID, Expected: parent.Version, Target: target, ActorAgentID: root.AgentID}); !errors.Is(err, orchestration.ErrTaskNotReady) {
					t.Fatalf("parent advanced to %s before refinement completed: %v", target, err)
				}
			}
			for _, key := range []string{"implement", "verify"} {
				child, err := store.GetTask(ctx, plan.Children[key].ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, target := range []orchestration.TaskState{orchestration.TaskReady, orchestration.TaskClaimed, orchestration.TaskInProgress, orchestration.TaskCompleted} {
					child = refinementTaskTransition(t, store, child, root.AgentID, target)
				}
				if key == "implement" {
					if _, err := store.TransitionTask(ctx, orchestration.TaskTransition{TaskID: parent.ID, Expected: parent.Version, Target: orchestration.TaskCompleted, ActorAgentID: root.AgentID}); !errors.Is(err, orchestration.ErrTaskNotReady) {
						t.Fatalf("only one child completion opened parent gate: %v", err)
					}
				}
			}
			parent = refinementTaskTransition(t, store, parent, root.AgentID, orchestration.TaskVerification)
			refinementTaskTransition(t, store, parent, root.AgentID, orchestration.TaskCompleted)
		})
	}
}

func TestTaskRefinementDependencyEditsCannotCloseRefinementCycle(t *testing.T) {
	database, root, parent := refinementFixture(t)
	ctx, store := context.Background(), database.Orchestration()
	plan, err := store.RefineTasks(ctx, refinementSpec(root, parent))
	if err != nil {
		t.Fatal(err)
	}
	external, err := store.CreateTask(ctx, orchestration.TaskSpec{TeamID: root.TeamID, Title: "External dependent"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddDependency(ctx, external.ID, parent.ID, root.AgentID, "external-edge"); err != nil {
		t.Fatal(err)
	}
	for _, dependency := range []orchestration.TaskID{parent.ID, external.ID} {
		if err := store.AddDependency(ctx, plan.Children["implement"].ID, dependency, root.AgentID, "closing-edge"); !errors.Is(err, orchestration.ErrTaskDependencyCycle) {
			t.Fatalf("dependency edit closed refinement cycle via %s: %v", dependency, err)
		}
	}
	dependencies, err := store.ListDependencies(ctx, plan.Children["implement"].ID)
	if err != nil || len(dependencies) != 0 {
		t.Fatalf("rejected cycle persisted: %v %v", dependencies, err)
	}
}

func TestTaskRefinementRejectsCyclesAndRollsBackWholeIncrement(t *testing.T) {
	for _, cycle := range []string{"explicit", "implicit-parent"} {
		t.Run(cycle, func(t *testing.T) {
			database, root, parent := refinementFixture(t)
			store, ctx := database.Orchestration(), context.Background()
			spec := refinementSpec(root, parent)
			if cycle == "explicit" {
				spec.Dependencies = append(spec.Dependencies, orchestration.TaskRefinementDependency{ChildKey: "implement", DependencyChildKey: "verify"})
			} else {
				spec.Dependencies = append(spec.Dependencies, orchestration.TaskRefinementDependency{ChildKey: "implement", DependencyTaskID: parent.ID})
			}
			if _, err := store.RefineTasks(ctx, spec); !errors.Is(err, orchestration.ErrTaskDependencyCycle) {
				t.Fatalf("cycle accepted: %v", err)
			}
			tasks, err := store.ListTasks(ctx, orchestration.TaskQuery{TeamID: root.TeamID})
			if err != nil || len(tasks) != 1 || tasks[0].Version != parent.Version {
				t.Fatalf("failed refinement partially committed: %+v %v", tasks, err)
			}
			if _, err := store.RefineTasks(ctx, refinementSpec(root, parent)); err != nil {
				t.Fatalf("rollback left unusable idempotency key: %v", err)
			}
		})
	}
}

func TestTaskRefinementRequiresOwnershipAndFreshParentVersion(t *testing.T) {
	database, root, parent := refinementFixture(t)
	store, ctx := database.Orchestration(), context.Background()
	other, err := store.CreateChild(ctx, orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker-v1", InitialPrompt: "unrelated"})
	if err != nil {
		t.Fatal(err)
	}
	spec := refinementSpec(root, parent)
	spec.ActorAgentID = other.ID
	if _, err := store.RefineTasks(ctx, spec); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("unrelated worker refined parent: %v", err)
	}
	spec.ActorAgentID = root.AgentID
	parent, err = store.TransitionTask(ctx, orchestration.TaskTransition{TaskID: parent.ID, Expected: parent.Version, Target: orchestration.TaskReady})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RefineTasks(ctx, spec); !errors.Is(err, orchestration.ErrTaskStateConflict) {
		t.Fatalf("stale parent version accepted: %v", err)
	}
	owner, err := store.CreateChild(ctx, orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, TaskID: parent.ID, ProfileRevision: "worker-v1", InitialPrompt: "own this task"})
	if err != nil {
		t.Fatal(err)
	}
	parent, err = store.GetTask(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	spec.ActorAgentID, spec.Expected = owner.ID, parent.Version
	if _, err := store.RefineTasks(ctx, spec); err != nil {
		t.Fatalf("task assignee cannot refine: %v", err)
	}
}

func TestTaskRefinementPropagatesReceiptReadFailure(t *testing.T) {
	database, root, parent := refinementFixture(t)
	ctx := context.Background()
	spec := refinementSpec(root, parent)
	_, err := database.Orchestration().RefineTasks(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	// A broken receipt cannot silently return a partial result.
	if _, err := database.db.ExecContext(ctx, `UPDATE idempotency_records SET result_id = ? WHERE scope = ? AND idempotency_key = ?`, `{"implement":"missing","verify":"missing"}`, "task-refinement:"+string(root.TeamID), spec.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Orchestration().RefineTasks(ctx, spec); err == nil {
		t.Fatal("missing receipt children were ignored")
	}
}

func TestTaskRefinementReceiptSurvivesReopen(t *testing.T) {
	ctx, path := context.Background(), filepath.Join(t.TempDir(), "reopen.db")
	database, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	root, _, err := database.Orchestration().EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "refinement-reopen"})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := database.Orchestration().CreateTask(ctx, orchestration.TaskSpec{TeamID: root.TeamID, Title: "Parent"})
	if err != nil {
		t.Fatal(err)
	}
	spec := refinementSpec(root, parent)
	first, err := database.Orchestration().RefineTasks(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	result, err := reopened.Orchestration().RefineTasks(ctx, spec)
	if err != nil || result.Children["implement"].ID != first.Children["implement"].ID || result.Children["verify"].ID != first.Children["verify"].ID {
		t.Fatalf("reopen changed refinement: %+v %v", result, err)
	}
	tasks, err := reopened.Orchestration().ListTasks(ctx, orchestration.TaskQuery{TeamID: root.TeamID})
	if err != nil || len(tasks) != 3 {
		t.Fatalf("reopen repeated children: %+v %v", tasks, err)
	}
}

func TestTaskRefinementEventFailureRollsBackPlanAndReceipt(t *testing.T) {
	database, root, parent := refinementFixture(t)
	ctx := context.Background()
	if _, err := database.db.ExecContext(ctx, `CREATE TRIGGER reject_refinement_event BEFORE INSERT ON orchestration_events WHEN NEW.event_type = 'task.created' BEGIN SELECT RAISE(ABORT, 'injected refinement event failure'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Orchestration().RefineTasks(ctx, refinementSpec(root, parent)); err == nil {
		t.Fatal("event persistence failure was ignored")
	}
	tasks, err := database.Orchestration().ListTasks(ctx, orchestration.TaskQuery{TeamID: root.TeamID})
	if err != nil || len(tasks) != 1 || tasks[0].Version != parent.Version {
		t.Fatalf("event failure partially committed: %+v %v", tasks, err)
	}
	if _, err := database.db.ExecContext(ctx, `DROP TRIGGER reject_refinement_event`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Orchestration().RefineTasks(ctx, refinementSpec(root, parent)); err != nil {
		t.Fatalf("event rollback consumed command identity: %v", err)
	}
}

package atestore

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

type workspaceFixture struct {
	store *Store
	ctx   context.Context
	team  orchestration.TeamID
	agent orchestration.AgentID
	task  orchestration.TaskID
	view  workspacecoord.View
}

func workspaceNewFixture(t *testing.T) workspaceFixture {
	t.Helper()
	store := newTestStore(t)
	team, err := orchestration.NewTeamID()
	if err != nil {
		t.Fatal(err)
	}
	agent, err := orchestration.NewAgentID()
	if err != nil {
		t.Fatal(err)
	}
	task, err := orchestration.NewTaskID()
	if err != nil {
		t.Fatal(err)
	}
	ctx, now := context.Background(), time.Now().UTC()
	err = store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		if err := createRecord(tx, "team", string(team), orchestration.TeamSnapshot{ID: team, RootAgentID: agent, State: orchestration.TeamActive, Version: 1}); err != nil {
			return err
		}
		if err := createRecord(tx, "agent", string(agent), orchestration.AgentSnapshot{ID: agent, TeamID: team, State: orchestration.AgentRunning, Version: 1}); err != nil {
			return err
		}
		return createRecord(tx, "task", string(task), orchestration.TaskSnapshot{
			ID: task, TeamID: team, State: orchestration.TaskAwaitingMerge, AssigneeAgentID: agent,
			Version: 1, CreatedAt: now, UpdatedAt: now,
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	view, err := store.CreateView(ctx, workspacecoord.ViewSpec{
		TeamID: team, AgentID: agent, TaskID: task, Mode: workspacecoord.ViewSharedLease,
		WorkspaceRoot: root, RootPath: root, IdempotencyKey: "view",
	})
	if err != nil {
		t.Fatal(err)
	}
	return workspaceFixture{store: store, ctx: ctx, team: team, agent: agent, task: task, view: view}
}

func (f workspaceFixture) spec(key string) workspacecoord.MergeSpec {
	return workspacecoord.MergeSpec{TeamID: f.team, AgentID: f.agent, TaskID: f.task,
		ViewID: f.view.ID, BaseRevision: "base", PatchArtifact: "sha256:patch", IdempotencyKey: key}
}

func (f workspaceFixture) enqueue(t *testing.T, key string) workspacecoord.MergeRequest {
	t.Helper()
	merge, err := f.store.EnqueueMerge(f.ctx, f.spec(key))
	if err != nil {
		t.Fatal(err)
	}
	return merge
}

func (f workspaceFixture) transition(t *testing.T, merge workspacecoord.MergeRequest, state workspacecoord.MergeState) workspacecoord.MergeRequest {
	t.Helper()
	updated, err := f.store.TransitionMerge(f.ctx, workspacecoord.MergeTransition{MergeID: merge.ID, Expected: merge.Version, Target: state, ActorAgentID: f.agent})
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func workspaceEvents(t *testing.T, store *Store) []orchestration.Event {
	t.Helper()
	var events []orchestration.Event
	if err := store.authority.View(context.Background(), func(tx *ateauthority.Tx) error {
		var err error
		events, err = listRecords[orchestration.Event](tx, "event")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return events
}

func TestWorkspaceRepositoryReplayAndVersioning(t *testing.T) {
	f := workspaceNewFixture(t)
	spec := workspacecoord.ViewSpec{
		TeamID: f.team, AgentID: f.agent, TaskID: f.task, Mode: f.view.Mode,
		WorkspaceRoot: f.view.WorkspaceRoot, RootPath: f.view.RootPath, IdempotencyKey: "view", CorrelationID: "different-correlation",
	}
	before := workspaceEvents(t, f.store)
	view, err := f.store.CreateView(nil, spec)
	if err != nil || !reflect.DeepEqual(view, f.view) {
		t.Fatalf("view replay = %#v, %v", view, err)
	}
	spec.Mode = workspacecoord.ViewSharedReadonly
	if _, err := f.store.CreateView(f.ctx, spec); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("changed view identity = %v", err)
	}
	if !reflect.DeepEqual(before, workspaceEvents(t, f.store)) {
		t.Fatal("view replay/conflict appended events")
	}
	merge := f.enqueue(t, "merge")
	replay := f.spec("merge")
	replay.CorrelationID = "another-correlation"
	repeated, err := f.store.EnqueueMerge(f.ctx, replay)
	if err != nil || !reflect.DeepEqual(repeated, merge) {
		t.Fatalf("merge replay = %#v, %v", repeated, err)
	}
	replay.PatchArtifact = "changed"
	if _, err := f.store.EnqueueMerge(f.ctx, replay); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("changed merge identity = %v", err)
	}
	running := f.transition(t, merge, workspacecoord.MergeRunning)
	if running.Version != 2 {
		t.Fatalf("running version = %d", running.Version)
	}
	if _, err := f.store.TransitionMerge(f.ctx, workspacecoord.MergeTransition{MergeID: merge.ID, Expected: 1, Target: workspacecoord.MergeApplied}); !errors.Is(err, orchestration.ErrSequenceConflict) {
		t.Fatalf("stale transition = %v", err)
	}
	applied := f.transition(t, running, workspacecoord.MergeApplied)
	if _, err := f.store.TransitionMerge(f.ctx, workspacecoord.MergeTransition{MergeID: merge.ID, Target: workspacecoord.MergeRunning}); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("terminal transition = %v", err)
	}
	got, err := f.store.GetMerge(f.ctx, merge.ID)
	if err != nil || !reflect.DeepEqual(got, applied) {
		t.Fatalf("get merge = %#v, %v", got, err)
	}
	merges, err := f.store.ListMerges(f.ctx, workspacecoord.MergeQuery{TeamID: f.team, ViewID: f.view.ID, TaskID: f.task, States: []workspacecoord.MergeState{workspacecoord.MergeApplied}})
	if err != nil || len(merges) != 1 || merges[0].ID != merge.ID {
		t.Fatalf("filtered merges = %#v, %v", merges, err)
	}
	released, err := f.store.ReleaseView(f.ctx, view.ID, "done")
	if err != nil || released.State != workspacecoord.ViewReleased || released.ReleasedAt == nil {
		t.Fatalf("release = %#v, %v", released, err)
	}
	before = workspaceEvents(t, f.store)
	releasedAgain, err := f.store.ReleaseView(f.ctx, view.ID, "repeat")
	if err != nil || !reflect.DeepEqual(releasedAgain, released) || !reflect.DeepEqual(before, workspaceEvents(t, f.store)) {
		t.Fatalf("release replay = %#v, %v", releasedAgain, err)
	}
	if _, err := f.store.EnqueueMerge(f.ctx, f.spec("released")); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("enqueue released view = %v", err)
	}
	views, err := f.store.ListViews(f.ctx, workspacecoord.ViewQuery{TeamID: f.team, AgentID: f.agent, States: []workspacecoord.ViewState{workspacecoord.ViewReleased}})
	if err != nil || len(views) != 1 || views[0].ID != view.ID {
		t.Fatalf("filtered views = %#v, %v", views, err)
	}
}

func TestWorkspaceMergeSupersessionAndResolution(t *testing.T) {
	f := workspaceNewFixture(t)
	original := f.enqueue(t, "original")
	replacement := f.spec("replacement")
	replacement.Supersedes = original.ID
	replacement.PatchArtifact = "replacement"
	next, err := f.store.EnqueueMerge(f.ctx, replacement)
	if err != nil {
		t.Fatal(err)
	}
	previous, err := f.store.GetMerge(f.ctx, original.ID)
	if err != nil || previous.State != workspacecoord.MergeSuperseded || previous.SupersededBy != next.ID || previous.Version != 2 {
		t.Fatalf("previous = %#v, %v", previous, err)
	}
	before := workspaceEvents(t, f.store)
	replay, err := f.store.EnqueueMerge(f.ctx, replacement)
	if err != nil || replay.ID != next.ID || !reflect.DeepEqual(before, workspaceEvents(t, f.store)) {
		t.Fatalf("replacement replay = %#v, %v", replay, err)
	}
	replacement.Supersedes = ""
	if _, err := f.store.EnqueueMerge(f.ctx, replacement); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("changed replacement relation = %v", err)
	}
	next = f.transition(t, next, workspacecoord.MergeRunning)
	next = f.transition(t, next, workspacecoord.MergeConflicted)
	taskID, err := orchestration.NewTaskID()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.authority.Update(f.ctx, func(tx *ateauthority.Tx) error {
		return createRecord(tx, "task", string(taskID), orchestration.TaskSnapshot{ID: taskID, TeamID: f.team, State: orchestration.TaskReady, Version: 1})
	}); err != nil {
		t.Fatal(err)
	}
	linked, err := f.store.LinkMergeResolutionTask(f.ctx, next.ID, taskID)
	if err != nil || linked.ResolutionTaskID != taskID || linked.Version != next.Version || !linked.UpdatedAt.Equal(next.UpdatedAt) {
		t.Fatalf("link = %#v, %v", linked, err)
	}
	before = workspaceEvents(t, f.store)
	if _, err := f.store.LinkMergeResolutionTask(f.ctx, next.ID, taskID); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, workspaceEvents(t, f.store)) {
		t.Fatal("resolution replay emitted another event")
	}
	if _, err := f.store.LinkMergeResolutionTask(f.ctx, next.ID, f.task); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("replace link = %v", err)
	}
	transition := workspacecoord.MergeTransition{MergeID: next.ID, Expected: next.Version, Target: workspacecoord.MergePending, ResolutionTaskID: taskID}
	if _, err := f.store.TransitionMerge(f.ctx, transition); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("incomplete resolution = %v", err)
	}
	if err := f.store.authority.Update(f.ctx, func(tx *ateauthority.Tx) error {
		task, err := getRecord[orchestration.TaskSnapshot](tx, "task", string(taskID))
		if err != nil {
			return err
		}
		task.State = orchestration.TaskCompleted
		return putRecord(tx, "task", string(taskID), task)
	}); err != nil {
		t.Fatal(err)
	}
	requeued, err := f.store.TransitionMerge(f.ctx, transition)
	if err != nil || requeued.State != workspacecoord.MergePending {
		t.Fatalf("requeue = %#v, %v", requeued, err)
	}
	merges, err := f.store.ListMerges(f.ctx, workspacecoord.MergeQuery{TeamID: f.team, ResolutionTaskID: taskID})
	if err != nil || len(merges) != 1 || merges[0].ID != next.ID {
		t.Fatalf("resolution query = %#v, %v", merges, err)
	}
}

func TestWorkspaceRejectedRelationsRollbackQueueAndEvents(t *testing.T) {
	f := workspaceNewFixture(t)
	original := f.enqueue(t, "original")
	original = f.transition(t, original, workspacecoord.MergeRunning)
	before := workspaceEvents(t, f.store)
	spec := f.spec("retryable-key")
	spec.Supersedes = original.ID
	if _, err := f.store.EnqueueMerge(f.ctx, spec); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("supersede running = %v", err)
	}
	merges, err := f.store.ListMerges(f.ctx, workspacecoord.MergeQuery{TeamID: f.team})
	if err != nil || len(merges) != 1 || !reflect.DeepEqual(before, workspaceEvents(t, f.store)) {
		t.Fatalf("failed enqueue leaked records/events: %#v, %v", merges, err)
	}
	spec.Supersedes = ""
	if _, err := f.store.EnqueueMerge(f.ctx, spec); err != nil {
		t.Fatalf("failed enqueue reserved idempotency key: %v", err)
	}
	spec = f.spec("wrong-task")
	spec.TaskID = ""
	if _, err := f.store.EnqueueMerge(f.ctx, spec); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("view/task mismatch = %v", err)
	}
}

func TestWorkspaceLegacyIdentitiesCannotChangeRelations(t *testing.T) {
	f := workspaceNewFixture(t)
	spec := workspacecoord.ViewSpec{
		TeamID: f.team, AgentID: f.agent, TaskID: f.task, Mode: f.view.Mode,
		WorkspaceRoot: f.view.WorkspaceRoot, RootPath: f.view.RootPath, IdempotencyKey: "view",
	}
	if err := f.store.authority.Update(f.ctx, func(tx *ateauthority.Tx) error {
		return putRecord(tx, "workspace-view-key", workspaceIdentityID(f.team, "view"), workspaceIdentity{ID: string(f.view.ID)})
	}); err != nil {
		t.Fatal(err)
	}
	if view, err := f.store.CreateView(f.ctx, spec); err != nil || view.ID != f.view.ID {
		t.Fatalf("legacy view replay = %#v, %v", view, err)
	}
	spec.TaskID = ""
	if _, err := f.store.CreateView(f.ctx, spec); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("legacy view changed task = %v", err)
	}
	old := f.enqueue(t, "old")
	replacement := f.spec("replacement")
	replacement.Supersedes = old.ID
	next, err := f.store.EnqueueMerge(f.ctx, replacement)
	if err != nil {
		t.Fatal(err)
	}
	legacy := replacement
	legacy.Supersedes = ""
	for _, hash := range []string{"", mergeSpecHash(legacy)} {
		if err := f.store.authority.Update(f.ctx, func(tx *ateauthority.Tx) error {
			return putRecord(tx, "merge-key", workspaceIdentityID(f.team, "replacement"), workspaceIdentity{ID: string(next.ID), Hash: hash})
		}); err != nil {
			t.Fatal(err)
		}
		if replay, err := f.store.EnqueueMerge(f.ctx, replacement); err != nil || replay.ID != next.ID {
			t.Fatalf("legacy merge replay = %#v, %v", replay, err)
		}
		if _, err := f.store.EnqueueMerge(f.ctx, legacy); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
			t.Fatalf("legacy merge removed replacement = %v", err)
		}
	}
}

func TestWorkspaceRejectsForeignTaskAndResolution(t *testing.T) {
	f := workspaceNewFixture(t)
	otherTeam, err := orchestration.NewTeamID()
	if err != nil {
		t.Fatal(err)
	}
	otherTask, err := orchestration.NewTaskID()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.authority.Update(f.ctx, func(tx *ateauthority.Tx) error {
		return createRecord(tx, "task", string(otherTask), orchestration.TaskSnapshot{ID: otherTask, TeamID: otherTeam, State: orchestration.TaskCompleted, Version: 1})
	}); err != nil {
		t.Fatal(err)
	}
	_, err = f.store.CreateView(f.ctx, workspacecoord.ViewSpec{
		TeamID: f.team, AgentID: f.agent, TaskID: otherTask, Mode: f.view.Mode,
		WorkspaceRoot: f.view.WorkspaceRoot, RootPath: f.view.RootPath,
	})
	if !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("foreign task view = %v", err)
	}
	merge := f.transition(t, f.enqueue(t, "conflict"), workspacecoord.MergeRunning)
	merge = f.transition(t, merge, workspacecoord.MergeConflicted)
	if _, err := f.store.LinkMergeResolutionTask(f.ctx, merge.ID, otherTask); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("foreign resolution task = %v", err)
	}
	missing, err := orchestration.NewTaskID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.LinkMergeResolutionTask(f.ctx, merge.ID, missing); !errors.Is(err, orchestration.ErrNotFound) {
		t.Fatalf("missing resolution task = %v", err)
	}
}

func TestWorkspaceConcurrentReplayAndMergeCAS(t *testing.T) {
	f := workspaceNewFixture(t)
	var results [2]workspacecoord.MergeRequest
	var errs [2]error
	var workers sync.WaitGroup
	start := make(chan struct{})
	for i := range results {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			results[i], errs[i] = f.store.EnqueueMerge(f.ctx, f.spec("concurrent"))
		}()
	}
	close(start)
	workers.Wait()
	if errs[0] != nil || errs[1] != nil || results[0].ID != results[1].ID {
		t.Fatalf("concurrent replay = %#v, %v", results, errs)
	}
	events := workspaceEvents(t, f.store)
	queued := 0
	for _, event := range events {
		if event.Type == orchestration.EventMergeQueued {
			queued++
		}
	}
	if queued != 1 {
		t.Fatalf("concurrent enqueue emitted %d queue events", queued)
	}
	start = make(chan struct{})
	for i := range results {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			_, errs[i] = f.store.TransitionMerge(f.ctx, workspacecoord.MergeTransition{
				MergeID: results[i].ID, Expected: 1, Target: workspacecoord.MergeRunning,
			})
		}()
	}
	close(start)
	workers.Wait()
	success, conflict := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			success++
		case errors.Is(err, orchestration.ErrSequenceConflict):
			conflict++
		default:
			t.Fatalf("concurrent transition = %v", err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("concurrent CAS success=%d conflict=%d", success, conflict)
	}
}

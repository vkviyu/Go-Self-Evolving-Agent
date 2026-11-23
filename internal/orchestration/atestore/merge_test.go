package atestore

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

func TestMergeApplyJournalIndividualProgressAndRestart(t *testing.T) {
	f := workspaceNewFixture(t)
	merge := f.enqueue(t, "apply")
	plan := workspacecoord.ApplyPlan{Version: 1, MergeID: merge.ID, Digest: "sha256:plan",
		Entries: []workspacecoord.PlanEntry{{Path: "a/b.txt", Action: workspacecoord.ActionWrite}, {Path: "a", Action: workspacecoord.ActionDelete}}}
	if _, err := f.store.BeginApply(f.ctx, plan); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("begin pending merge = %v", err)
	}
	f.transition(t, merge, workspacecoord.MergeRunning)
	record, err := f.store.BeginApply(f.ctx, plan)
	if err != nil || record.State != workspacecoord.ApplyStarted || len(record.Applied) != 0 {
		t.Fatalf("begin = %#v, %v", record, err)
	}
	for _, path := range []string{"a/b.txt", "a", "a/b.txt"} {
		if err := f.store.MarkPathApplied(f.ctx, merge.ID, path); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.store.authority.View(f.ctx, func(tx *ateauthority.Tx) error {
		paths, err := listRecords[mergeApplyPath](tx, mergeApplyPathKind(merge.ID))
		if err == nil && len(paths) != 2 {
			t.Fatalf("individual progress records = %d", len(paths))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// A fresh adapter must reconstruct every fact from authority records.
	restarted := &Store{authority: f.store.authority, sessions: f.store.sessions}
	record, found, err := restarted.LoadApply(f.ctx, merge.ID)
	if err != nil || !found || len(record.Applied) != 2 || !record.Applied["a/b.txt"] || !reflect.DeepEqual(record.Plan, plan) {
		t.Fatalf("restart load = %#v, found=%v, %v", record, found, err)
	}
	record.Applied["caller-only"] = true
	record.Plan.Entries[0].Path = "caller-mutated"
	replay, err := restarted.BeginApply(f.ctx, plan)
	if err != nil || replay.Applied["caller-only"] || replay.Plan.Entries[0].Path != "a/b.txt" {
		t.Fatalf("replay aliased result = %#v, %v", replay, err)
	}
	different := plan
	different.Digest = "different"
	if _, err := restarted.BeginApply(f.ctx, different); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("changed digest = %v", err)
	}
	for range 2 {
		if err := restarted.FinishApply(f.ctx, merge.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := restarted.MarkPathApplied(f.ctx, merge.ID, "a"); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("finished progress = %v", err)
	}
	record, err = restarted.BeginApply(f.ctx, plan)
	if err != nil || record.State != workspacecoord.ApplyFinished || len(record.Applied) != 2 {
		t.Fatalf("finished replay = %#v, %v", record, err)
	}
	missing, err := orchestration.NewMergeID()
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := restarted.LoadApply(f.ctx, missing); err != nil || found {
		t.Fatalf("missing load = %v, %v", found, err)
	}
	if err := restarted.FinishApply(f.ctx, missing); !errors.Is(err, orchestration.ErrNotFound) {
		t.Fatalf("missing finish = %v", err)
	}
	if err := restarted.MarkPathApplied(f.ctx, missing, "path"); !errors.Is(err, orchestration.ErrNotFound) {
		t.Fatalf("missing progress = %v", err)
	}
}

func mergePrepareReceipt(t *testing.T, f workspaceFixture, merge workspacecoord.MergeRequest) workspacecoord.MergeFinalization {
	t.Helper()
	receipt, err := f.store.BeginMergeFinalization(f.ctx, workspacecoord.MergeFinalization{
		MergeID: merge.ID, BindingDigest: workspacecoord.MergeBindingDigest(merge),
		WorkspaceDigest: "sha256:workspace", VerificationDigest: "verifier-v1", State: workspacecoord.FinalizationPrepared,
	})
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func TestMergeFinalizationStateMachineAndUnknownRecovery(t *testing.T) {
	f := workspaceNewFixture(t)
	merge := f.transition(t, f.enqueue(t, "receipt"), workspacecoord.MergeRunning)
	receipt := mergePrepareReceipt(t, f, merge)
	repeated, err := f.store.BeginMergeFinalization(f.ctx, receipt)
	if err != nil || repeated != receipt {
		t.Fatalf("prepare replay = %#v, %v", repeated, err)
	}
	for _, change := range []func(*workspacecoord.MergeFinalization){
		func(r *workspacecoord.MergeFinalization) { r.BindingDigest = "changed" },
		func(r *workspacecoord.MergeFinalization) { r.WorkspaceDigest = "changed" },
		func(r *workspacecoord.MergeFinalization) { r.VerificationDigest = "changed" },
	} {
		changed := receipt
		change(&changed)
		if _, err := f.store.BeginMergeFinalization(f.ctx, changed); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
			t.Fatalf("changed prepare identity = %v", err)
		}
	}
	if err := f.store.SettleMergeFinalization(f.ctx, merge.ID, receipt.BindingDigest); !errors.Is(err, orchestration.ErrSequenceConflict) {
		t.Fatalf("settle prepared = %v", err)
	}
	receipt.State = workspacecoord.FinalizationDispatched
	if _, err := f.store.TransitionMergeFinalization(f.ctx, receipt, workspacecoord.FinalizationPrepared); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.TransitionMergeFinalization(f.ctx, receipt, workspacecoord.FinalizationPrepared); err != nil {
		t.Fatalf("dispatch replay = %v", err)
	}
	restarted := &Store{authority: f.store.authority, sessions: f.store.sessions}
	loaded, found, err := restarted.LoadMergeFinalization(f.ctx, merge.ID)
	if err != nil || !found || loaded != receipt {
		t.Fatalf("dispatched restart = %#v, %v, %v", loaded, found, err)
	}
	receipt.State, receipt.TargetState, receipt.Reason = workspacecoord.FinalizationUnknown, orchestration.TaskFailed, "interrupted; do not rerun"
	if _, err := restarted.TransitionMergeFinalization(f.ctx, receipt, workspacecoord.FinalizationDispatched); err != nil {
		t.Fatal(err)
	}
	stale := receipt
	stale.State = workspacecoord.FinalizationCompleted
	if _, err := restarted.TransitionMergeFinalization(f.ctx, stale, workspacecoord.FinalizationPrepared); !errors.Is(err, orchestration.ErrSequenceConflict) {
		t.Fatalf("stale completion = %v", err)
	}
	f.transition(t, merge, workspacecoord.MergeApplied)
	if err := restarted.SettleMergeFinalization(f.ctx, merge.ID, receipt.BindingDigest); err != nil {
		t.Fatal(err)
	}
	task, err := restarted.GetTask(f.ctx, f.task)
	if err != nil || task.State != orchestration.TaskFailed {
		t.Fatalf("unknown task = %#v, %v", task, err)
	}
	loaded, found, err = restarted.LoadMergeFinalization(f.ctx, merge.ID)
	if err != nil || !found || !loaded.Settled || loaded.State != workspacecoord.FinalizationUnknown {
		t.Fatalf("settled unknown = %#v, %v", loaded, err)
	}
	if _, err := restarted.TransitionMergeFinalization(f.ctx, receipt, workspacecoord.FinalizationDispatched); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("transition settled receipt = %v", err)
	}
}

func TestMergeFinalizationAtomicSettlementRollbackAndReplay(t *testing.T) {
	f := workspaceNewFixture(t)
	previous := f.enqueue(t, "old")
	spec := f.spec("new")
	spec.Supersedes = previous.ID
	replacement, err := f.store.EnqueueMerge(f.ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	replacement = f.transition(t, replacement, workspacecoord.MergeRunning)
	replacement = f.transition(t, replacement, workspacecoord.MergeApplied)
	receipt := mergePrepareReceipt(t, f, replacement)
	receipt.State, receipt.TargetState, receipt.Reason = workspacecoord.FinalizationCompleted, orchestration.TaskCompleted, "verified"
	if _, err := f.store.TransitionMergeFinalization(f.ctx, receipt, workspacecoord.FinalizationPrepared); err != nil {
		t.Fatal(err)
	}
	pending, err := f.store.PendingMergeFinalizations(f.ctx, f.team, 1)
	if err != nil || len(pending) != 1 || pending[0].ID != replacement.ID {
		t.Fatalf("pending = %#v, %v", pending, err)
	}
	// Break a later chain relation. Settlement stages the new task/view first;
	// failure on the predecessor must roll back those facts and their events.
	var old workspacecoord.MergeRequest
	if err := f.store.authority.Update(f.ctx, func(tx *ateauthority.Tx) error {
		var err error
		old, err = getRecord[workspacecoord.MergeRequest](tx, mergeRecordKind, string(previous.ID))
		if err != nil {
			return err
		}
		broken := old
		broken.SupersededBy = ""
		return putRecord(tx, mergeRecordKind, string(old.ID), broken)
	}); err != nil {
		t.Fatal(err)
	}
	before := workspaceEvents(t, f.store)
	if err := f.store.SettleMergeFinalization(f.ctx, replacement.ID, receipt.BindingDigest); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("broken chain settlement = %v", err)
	}
	task, err := f.store.GetTask(f.ctx, f.task)
	if err != nil || task.State != orchestration.TaskAwaitingMerge || task.Version != 1 {
		t.Fatalf("rolled back task = %#v, %v", task, err)
	}
	view, err := f.store.GetView(f.ctx, f.view.ID)
	if err != nil || view.State != workspacecoord.ViewActive || !reflect.DeepEqual(before, workspaceEvents(t, f.store)) {
		t.Fatalf("rolled back view/events = %#v, %v", view, err)
	}
	loaded, found, err := f.store.LoadMergeFinalization(f.ctx, replacement.ID)
	if err != nil || !found || loaded.Settled {
		t.Fatalf("rolled back receipt = %#v, %v", loaded, err)
	}
	if err := f.store.authority.Update(f.ctx, func(tx *ateauthority.Tx) error {
		return putRecord(tx, mergeRecordKind, string(old.ID), old)
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SettleMergeFinalization(f.ctx, replacement.ID, receipt.BindingDigest); err != nil {
		t.Fatal(err)
	}
	task, err = f.store.GetTask(f.ctx, f.task)
	if err != nil || task.State != orchestration.TaskCompleted || task.Version != 2 {
		t.Fatalf("settled task = %#v, %v", task, err)
	}
	view, err = f.store.GetView(f.ctx, f.view.ID)
	if err != nil || view.State != workspacecoord.ViewReleased {
		t.Fatalf("settled view = %#v, %v", view, err)
	}
	before = workspaceEvents(t, f.store)
	if err := f.store.SettleMergeFinalization(f.ctx, replacement.ID, receipt.BindingDigest); err != nil || !reflect.DeepEqual(before, workspaceEvents(t, f.store)) {
		t.Fatalf("settlement replay = %v", err)
	}
	if err := f.store.SettleMergeFinalization(f.ctx, replacement.ID, "wrong"); !errors.Is(err, orchestration.ErrSequenceConflict) {
		t.Fatalf("wrong settlement binding = %v", err)
	}
	pending, err = f.store.PendingMergeFinalizations(f.ctx, f.team, 0)
	if err != nil || len(pending) != 0 {
		t.Fatalf("settled recovery query = %#v, %v", pending, err)
	}
}

func TestMergeFailedSnapshotPreservesView(t *testing.T) {
	f := workspaceNewFixture(t)
	spec := f.spec("snapshot")
	spec.Kind, spec.CandidateDigest = workspacecoord.MergeSnapshot, "sha256:candidate"
	merge, err := f.store.EnqueueMerge(f.ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	merge = f.transition(t, merge, workspacecoord.MergeRunning)
	merge = f.transition(t, merge, workspacecoord.MergeApplied)
	receipt := mergePrepareReceipt(t, f, merge)
	receipt.State, receipt.TargetState = workspacecoord.FinalizationCompleted, orchestration.TaskFailed
	if _, err := f.store.TransitionMergeFinalization(f.ctx, receipt, workspacecoord.FinalizationPrepared); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SettleMergeFinalization(context.Background(), merge.ID, receipt.BindingDigest); err != nil {
		t.Fatal(err)
	}
	view, err := f.store.GetView(f.ctx, f.view.ID)
	if err != nil || view.State != workspacecoord.ViewActive {
		t.Fatalf("failed candidate view = %#v, %v", view, err)
	}
}

func TestMergePendingFinalizationsIncludesMissingReceiptsInStableOrder(t *testing.T) {
	f := workspaceNewFixture(t)
	first := f.transition(t, f.enqueue(t, "first"), workspacecoord.MergeRunning)
	first = f.transition(t, first, workspacecoord.MergeApplied)
	second := f.transition(t, f.enqueue(t, "second"), workspacecoord.MergeRunning)
	second = f.transition(t, second, workspacecoord.MergeApplied)
	mergePrepareReceipt(t, f, second)
	// Equal creation times exercise the ID tie breaker, independent of key
	// encoding and insertion order.
	if err := f.store.authority.Update(f.ctx, func(tx *ateauthority.Tx) error {
		for _, merge := range []workspacecoord.MergeRequest{first, second} {
			merge.CreatedAt = time.Unix(1, 0).UTC()
			if err := putRecord(tx, mergeRecordKind, string(merge.ID), merge); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []orchestration.MergeID{first.ID, second.ID}
	slices.Sort(want)
	for _, limit := range []int{0, -1, 1001, 2} {
		pending, err := f.store.PendingMergeFinalizations(f.ctx, f.team, limit)
		if err != nil || len(pending) != 2 || pending[0].ID != want[0] || pending[1].ID != want[1] {
			t.Fatalf("limit %d pending = %#v, %v", limit, pending, err)
		}
	}
	pending, err := f.store.PendingMergeFinalizations(f.ctx, f.team, 1)
	if err != nil || len(pending) != 1 || pending[0].ID != want[0] {
		t.Fatalf("bounded pending = %#v, %v", pending, err)
	}
	other, err := orchestration.NewTeamID()
	if err != nil {
		t.Fatal(err)
	}
	if pending, err := f.store.PendingMergeFinalizations(f.ctx, other, 1); err != nil || len(pending) != 0 {
		t.Fatalf("other team pending = %#v, %v", pending, err)
	}
}

func TestMergeSettlementRetainsPreservedWorktreeIdentity(t *testing.T) {
	f := workspaceNewFixture(t)
	// Only record metadata is involved: no worktree or filesystem operations
	// may run inside the authority transaction.
	if err := f.store.authority.Update(f.ctx, func(tx *ateauthority.Tx) error {
		view := f.view
		view.Mode = workspacecoord.ViewGitWorktree
		return putRecord(tx, workspaceViewKind, string(view.ID), view)
	}); err != nil {
		t.Fatal(err)
	}
	merge := f.transition(t, f.enqueue(t, "worktree"), workspacecoord.MergeRunning)
	merge = f.transition(t, merge, workspacecoord.MergeApplied)
	receipt := mergePrepareReceipt(t, f, merge)
	receipt.State, receipt.TargetState = workspacecoord.FinalizationCompleted, orchestration.TaskCompleted
	if _, err := f.store.TransitionMergeFinalization(f.ctx, receipt, workspacecoord.FinalizationPrepared); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SettleMergeFinalization(f.ctx, merge.ID, receipt.BindingDigest); err != nil {
		t.Fatal(err)
	}
	view, err := f.store.GetView(f.ctx, f.view.ID)
	if err != nil || view.State != workspacecoord.ViewActive {
		t.Fatalf("preserved worktree identity = %#v, %v", view, err)
	}
}

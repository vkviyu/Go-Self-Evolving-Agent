package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentflow"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/plugin"
	"github.com/vkviyu/go-self-evolving-agent/internal/pluginbuild"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

func TestIgnoredConditionalUpdatesDoNotPublishTransitions(t *testing.T) {
	for _, kind := range []string{"agent", "team", "task", "goal"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			database := abandonmentDatabase(t)
			store := database.Orchestration()
			root, _, err := store.EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "cas-root"})
			if err != nil {
				t.Fatal(err)
			}
			var run func() error
			var table string
			var want error
			switch kind {
			case "agent":
				table, want = "agents", orchestration.ErrAgentStateConflict
				run = func() error {
					_, err := store.TransitionAgent(ctx, orchestration.AgentTransition{AgentID: root.AgentID, Expected: 1, Target: orchestration.AgentRunning})
					return err
				}
			case "team":
				table, want = "teams", orchestration.ErrTeamStateConflict
				run = func() error {
					_, err := store.TransitionTeam(ctx, orchestration.TeamTransition{TeamID: root.TeamID, Expected: 1, Target: orchestration.TeamCanceling})
					return err
				}
			case "task":
				task, err := store.CreateTask(ctx, orchestration.TaskSpec{TeamID: root.TeamID, Title: "CAS task"})
				if err != nil {
					t.Fatal(err)
				}
				table, want = "tasks", orchestration.ErrTaskStateConflict
				run = func() error {
					_, err := store.TransitionTask(ctx, orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskReady})
					return err
				}
			case "goal":
				_, err := store.CreateGoal(ctx, agentgoal.CreateRequest{ID: "cas-goal", SessionID: "cas-root", Text: "CAS goal", Assessment: agentgoal.AssessmentSelf, MaxRounds: 3, MaxWallTime: time.Minute})
				if err != nil {
					t.Fatal(err)
				}
				table, want = "goal_runs", orchestration.ErrSequenceConflict
				run = func() error { _, err := store.StartRound(ctx, "cas-goal", 1, "round", "input"); return err }
			}
			if _, err := database.db.ExecContext(ctx, fmt.Sprintf(`CREATE TRIGGER ignore_cas BEFORE UPDATE ON %s BEGIN SELECT RAISE(IGNORE); END`, table)); err != nil {
				t.Fatal(err)
			}
			before := snapshotSQLiteRows(t, ctx, database.db)
			if err := run(); !errors.Is(err, want) {
				t.Fatalf("got %v, want %v", err, want)
			}
			if got := snapshotSQLiteRows(t, ctx, database.db); got != before {
				t.Fatal("lost conditional update published events or dependent rows")
			}
		})
	}
}

func TestMissingRecordsExposeDomainNotFound(t *testing.T) {
	ctx := context.Background()
	database := abandonmentDatabase(t)
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "missing-records"})
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]func() error{
		"goal-start": func() error { _, err := store.StartRound(ctx, "missing", 1, "turn", "input"); return err },
		"goal-round-finish": func() error {
			_, err := store.FinishRound(ctx, "missing", 1, agentgoal.Verdict{Decision: agentgoal.DecisionContinue, Reason: "test"}, "")
			return err
		},
		"goal-finish": func() error {
			_, err := store.FinishGoal(ctx, "missing", agentgoal.Finish{State: agentgoal.StateFailed})
			return err
		},
		"flow-start-step": func() error { _, err := store.StartStep(ctx, "missing", "step", "hash"); return err },
		"flow-finish": func() error {
			_, err := store.FinishRun(ctx, "missing", agentflow.StateFailed, nil, "test")
			return err
		},
		"build": func() error { _, _, err := database.PluginBuild(ctx, "missing"); return err },
		"build-finish": func() error {
			return database.FinishPluginBuild(ctx, pluginbuild.Result{BuildID: "missing", State: "failed"})
		},
		"plugin-test": func() error { _, err := database.PluginTest(ctx, "missing"); return err },
		"plugin-activation": func() error {
			return database.Plugins().SetActive(ctx, "missing", "", "missing")
		},
		"strategy":        func() error { _, err := database.Strategies().Load(ctx, "missing"); return err },
		"strategy-suite":  func() error { _, err := database.Strategies().Suite(ctx, "missing"); return err },
		"strategy-report": func() error { _, err := database.Strategies().Report(ctx, "missing"); return err },
		"settlement": func() error {
			return store.StageSettlement(ctx, "missing", tokenusage.Record{TeamID: root.TeamID, AgentID: root.AgentID, RequestID: "request"})
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			err := call()
			if !errors.Is(err, orchestration.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("got %v, want domain NotFound without SQL sentinel", err)
			}
		})
	}
}

func TestPluginTestReportSaveIsIdempotentAndChecksExistingContent(t *testing.T) {
	ctx := context.Background()
	database := abandonmentDatabase(t)
	report := plugin.TestReport{Tool: "extension_test", Revision: "v1", ContentSHA256: "content", Results: []plugin.CaseResult{{Name: "case", Passed: true}}, Passed: true, TestedAt: time.Now().UTC()}
	id, err := database.SavePluginTest(ctx, report)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := database.SavePluginTest(ctx, report)
	if err != nil || repeated != id {
		t.Fatalf("repeated = %q, err=%v", repeated, err)
	}
	if _, err := database.db.ExecContext(ctx, `UPDATE plugin_test_reports SET report='{}' WHERE report_id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SavePluginTest(ctx, report); err == nil {
		t.Fatal("replayed save accepted corrupt existing evidence")
	}
}

func TestLegacyKeysCompareDurableRequestFields(t *testing.T) {
	ctx := context.Background()
	database := abandonmentDatabase(t)
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "legacy-identity"})
	if err != nil {
		t.Fatal(err)
	}
	spawn := orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker-v1", InitialPrompt: "original", IdempotencyKey: "spawn"}
	child, err := store.CreateChild(ctx, spawn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, `UPDATE idempotency_records SET request_hash='' WHERE idempotency_key='spawn'`); err != nil {
		t.Fatal(err)
	}
	if repeated, err := store.CreateChild(ctx, spawn); err != nil || repeated.ID != child.ID {
		t.Fatalf("legacy replay = %v, err=%v", repeated.ID, err)
	}
	spawn.InitialPrompt = "different"
	if _, err := store.CreateChild(ctx, spawn); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("changed legacy spawn accepted: %v", err)
	}
	if _, _, err := store.FindSpawnRequest(ctx, spawn); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("fast lookup accepted changed legacy spawn: %v", err)
	}
	taskSpec := orchestration.TaskSpec{TeamID: root.TeamID, ActorAgentID: root.AgentID, Title: "original", IdempotencyKey: "task"}
	task, err := store.CreateTask(ctx, taskSpec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, `UPDATE idempotency_records SET request_hash='' WHERE idempotency_key='task'`); err != nil {
		t.Fatal(err)
	}
	if repeated, err := store.CreateTask(ctx, taskSpec); err != nil || repeated.ID != task.ID {
		t.Fatalf("legacy task replay = %v, err=%v", repeated.ID, err)
	}
	taskSpec.Priority++
	if _, err := store.CreateTask(ctx, taskSpec); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("changed legacy task accepted: %v", err)
	}
	workspace := t.TempDir()
	viewSpec := workspacecoord.ViewSpec{TeamID: root.TeamID, AgentID: child.ID, Mode: workspacecoord.ViewSharedLease, WorkspaceRoot: workspace, RootPath: workspace, IdempotencyKey: "view"}
	view, err := store.CreateView(ctx, viewSpec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, `UPDATE workspace_views SET request_hash='' WHERE view_id=?`, string(view.ID)); err != nil {
		t.Fatal(err)
	}
	if repeated, err := store.CreateView(ctx, viewSpec); err != nil || repeated.ID != view.ID {
		t.Fatalf("legacy view replay = %v, err=%v", repeated.ID, err)
	}
	viewSpec.Mode = workspacecoord.ViewSharedReadonly
	if _, err := store.CreateView(ctx, viewSpec); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("changed legacy view accepted: %v", err)
	}
	mergeSpec := workspacecoord.MergeSpec{TeamID: root.TeamID, AgentID: child.ID, ViewID: view.ID, BaseRevision: "base", PatchArtifact: "sha256:patch", IdempotencyKey: "merge"}
	merge, err := store.EnqueueMerge(ctx, mergeSpec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, `UPDATE merge_queue SET request_hash='' WHERE merge_id=?`, string(merge.ID)); err != nil {
		t.Fatal(err)
	}
	if repeated, err := store.EnqueueMerge(ctx, mergeSpec); err != nil || repeated.ID != merge.ID {
		t.Fatalf("legacy merge replay = %v, err=%v", repeated.ID, err)
	}
	mergeSpec.PatchArtifact = "sha256:different"
	if _, err := store.EnqueueMerge(ctx, mergeSpec); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("changed legacy merge accepted: %v", err)
	}
}

func TestLegacyReservationCannotChangeOwner(t *testing.T) {
	ctx := context.Background()
	database := abandonmentDatabase(t)
	reservation, receipt := recoveryReservation(t, database, "legacy-owner")
	if _, err := database.db.ExecContext(ctx, `UPDATE usage_reservations SET request_hash='' WHERE reservation_id=?`, reservation.ID); err != nil {
		t.Fatal(err)
	}
	request := tokenusage.ReservationRequest{TeamID: receipt.TeamID, AgentID: receipt.AgentID, TurnID: receipt.TurnID, RequestID: receipt.RequestID,
		InputTokens: reservation.InputTokens, OutputTokens: reservation.OutputTokens, TotalTokens: reservation.TotalTokens, CostMicros: reservation.CostMicros,
		ModelProfile: receipt.ModelProfile, Provider: receipt.Provider, Model: receipt.Model}
	if _, err := database.Orchestration().Reserve(ctx, request); err != nil {
		t.Fatalf("same legacy reservation rejected: %v", err)
	}
	request.AgentID, _ = orchestration.NewAgentID()
	if _, err := database.Orchestration().Reserve(ctx, request); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("legacy reservation changed owner: %v", err)
	}
}

func TestCancelWaitSetsRollsBackTheWholeBatch(t *testing.T) {
	ctx := context.Background()
	database := abandonmentDatabase(t)
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "cancel-waits"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.CreateChild(ctx, orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker-v1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionAgent(ctx, orchestration.AgentTransition{AgentID: root.AgentID, Expected: 1, Target: orchestration.AgentRunning}); err != nil {
		t.Fatal(err)
	}
	first, err := store.CreateWaitSet(ctx, orchestration.WaitSpec{TeamID: root.TeamID, AgentID: root.AgentID, TargetAgentIDs: []orchestration.AgentID{child.ID}, Condition: "children"})
	if err != nil {
		t.Fatal(err)
	}
	second, _ := orchestration.NewWaitSetID()
	// Historical/interrupted runs can leave several pending waits. Seed the
	// second one directly so failure after canceling the first is observable.
	if _, err := database.db.ExecContext(ctx, `INSERT INTO wait_sets(wait_set_id,team_id,agent_id,condition,target_agent_ids,state,created_at,updated_at)
SELECT ?,team_id,agent_id,condition,target_agent_ids,state,created_at,updated_at FROM wait_sets WHERE wait_set_id=?`, string(second), string(first.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, `CREATE TRIGGER fail_second_cancel BEFORE INSERT ON orchestration_events WHEN NEW.event_type='wait_set.canceled' AND json_extract(NEW.payload,'$.wait_set_id')='`+string(second)+`' BEGIN SELECT RAISE(ABORT,'injected cancellation failure'); END`); err != nil {
		t.Fatal(err)
	}
	before := snapshotSQLiteRows(t, ctx, database.db)
	if err := store.CancelWaitSetsForAgent(ctx, root.AgentID, "test"); err == nil {
		t.Fatal("cancellation succeeded despite failed second event")
	}
	if got := snapshotSQLiteRows(t, ctx, database.db); got != before {
		t.Fatal("failed batch cancellation left partial states or events")
	}
}

func TestGitReplacementIdentityAndDeliveryScope(t *testing.T) {
	ctx := context.Background()
	database := abandonmentDatabase(t)
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "replacement-scope"})
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	view, err := store.CreateView(ctx, workspacecoord.ViewSpec{TeamID: root.TeamID, AgentID: root.AgentID, Mode: workspacecoord.ViewSharedLease, WorkspaceRoot: workspace, RootPath: workspace})
	if err != nil {
		t.Fatal(err)
	}
	spec := workspacecoord.MergeSpec{TeamID: root.TeamID, AgentID: root.AgentID, ViewID: view.ID, BaseRevision: "base", PatchArtifact: "sha256:first"}
	previous, err := store.EnqueueMerge(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	replacement := spec
	replacement.PatchArtifact, replacement.Supersedes, replacement.IdempotencyKey = "sha256:second", previous.ID, "replace"
	merge, err := store.EnqueueMerge(ctx, replacement)
	if err != nil {
		t.Fatal(err)
	}
	// An old git-patch request hash omitted Supersedes. It is still replayable
	// only when its persisted delivery relationship agrees exactly.
	oldRequest := replacement
	oldRequest.Supersedes = ""
	if _, err := database.db.ExecContext(ctx, `UPDATE merge_queue SET request_hash=? WHERE merge_id=?`, hashMergeSpec(oldRequest), string(merge.ID)); err != nil {
		t.Fatal(err)
	}
	if repeated, err := store.EnqueueMerge(ctx, replacement); err != nil || repeated.ID != merge.ID {
		t.Fatalf("old replacement replay = %v, err=%v", repeated.ID, err)
	}
	replacement.Supersedes = ""
	if _, err := store.EnqueueMerge(ctx, replacement); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("replacement relation was not bound to identity: %v", err)
	}
	otherView, err := store.CreateView(ctx, workspacecoord.ViewSpec{TeamID: root.TeamID, AgentID: root.AgentID, Mode: workspacecoord.ViewSharedLease, WorkspaceRoot: workspace, RootPath: workspace})
	if err != nil {
		t.Fatal(err)
	}
	replacement = spec
	replacement.ViewID, replacement.Supersedes, replacement.IdempotencyKey = otherView.ID, merge.ID, "unrelated"
	before := snapshotSQLiteRows(t, ctx, database.db)
	if _, err := store.EnqueueMerge(ctx, replacement); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("unrelated delivery superseded existing work: %v", err)
	}
	if got := snapshotSQLiteRows(t, ctx, database.db); got != before {
		t.Fatal("rejected supersession left a new merge or changed the old one")
	}
}

func TestFlowSessionBindingIsAtomicAndTeamScoped(t *testing.T) {
	ctx := context.Background()
	database := abandonmentDatabase(t)
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "binding-owner"})
	if err != nil {
		t.Fatal(err)
	}
	foreign, _, err := store.EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "binding-foreign"})
	if err != nil {
		t.Fatal(err)
	}
	input, _ := agentflow.NewValue("task")
	run, err := store.CreateRun(ctx, agentflow.RunRequest{ID: "binding-run", Definition: "binding", DefinitionRevision: "v1", DefinitionFingerprint: strings.Repeat("0", 64),
		SessionID: root.SessionID, TeamID: root.TeamID, AgentID: root.AgentID, IdempotencyKey: "binding-run", Input: input})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindSessionAgent(ctx, run.ID, "worker", foreign.AgentID); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("cross-Team binding accepted: %v", err)
	}
	if _, found, err := store.SessionAgent(ctx, run.ID, "worker"); err != nil || found {
		t.Fatalf("rejected binding remained durable: found=%v, err=%v", found, err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := store.BindSessionAgent(ctx, run.ID, "worker", root.AgentID); err != nil {
			t.Fatalf("binding attempt %d: %v", attempt, err)
		}
	}
}

func TestReviewerRejectionStartsANewTaskAttempt(t *testing.T) {
	ctx := context.Background()
	database := abandonmentDatabase(t)
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "review-reject"})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := store.CreateChild(ctx, orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker-v1"})
	if err != nil {
		t.Fatal(err)
	}
	reviewer, err := store.CreateChild(ctx, orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "reviewer-v1"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, orchestration.TaskSpec{TeamID: root.TeamID, Title: "review candidate", VerificationProfile: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []orchestration.TaskState{orchestration.TaskReady, orchestration.TaskClaimed, orchestration.TaskInProgress, orchestration.TaskVerification} {
		task, err = store.TransitionTask(ctx, orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: state, AssigneeAgentID: worker.ID, ActorAgentID: worker.ID})
		if err != nil {
			t.Fatal(err)
		}
	}
	previousAttempt := task.Attempt
	task, err = store.TransitionTask(ctx, orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskReady, ActorAgentID: reviewer.ID, Reason: "verification rejected"})
	if err != nil {
		t.Fatal(err)
	}
	if task.Attempt != previousAttempt+1 || task.AssigneeAgentID != "" {
		t.Fatalf("rejected candidate reused attempt or retained worker: %#v", task)
	}
}

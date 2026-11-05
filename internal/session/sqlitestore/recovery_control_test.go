package sqlitestore

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentflow"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func abandonmentDatabase(t *testing.T) *Database {
	t.Helper()
	database, err := Open(context.Background(), filepath.Join(t.TempDir(), "abandon.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func abandonmentRequest(turnID string) session.AbandonRequest {
	return session.AbandonRequest{SessionID: "root", TurnID: turnID, Reason: "Inspected workspace; stop recovery", EffectsReviewed: true}
}

func recordAbandonmentTurn(t *testing.T, database *Database, id session.ID, turnID, snapshotID string, completed bool) {
	t.Helper()
	items := []session.PendingEvent{{Type: session.EventTurnStarted, CorrelationID: turnID, RuntimeSnapshotID: snapshotID, Payload: session.MustJSONPayload(map[string]any{})}}
	if completed {
		items = append(items, session.PendingEvent{Type: session.EventTurnCompleted, CorrelationID: turnID, RuntimeSnapshotID: snapshotID, Payload: session.MustJSONPayload(map[string]any{"content": "candidate"})})
	}
	if _, err := database.SessionEvents().Append(context.Background(), id, session.AnySequence, items...); err != nil {
		t.Fatal(err)
	}
}

func createAbandonmentGoal(t *testing.T, database *Database) agentgoal.Goal {
	t.Helper()
	goal, err := database.Orchestration().CreateGoal(context.Background(), agentgoal.CreateRequest{ID: "goal", SessionID: "root", Text: "original task", Assessment: agentgoal.AssessmentSelf, MaxRounds: 3, MaxWallTime: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return goal
}

func TestAbandonRecoveryReleasesOnlyTargetAndPreservesEvidenceAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "abandon.db")
	database, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	manifest := runtimeManifest(t, "v1")
	id, _ := manifest.ID()
	ref := tool.SnapshotReference{Kind: "turn", Scope: "root", ID: "target"}
	for _, reference := range []tool.SnapshotReference{ref, {Kind: "turn", Scope: "root", ID: "other"}, {Kind: "flow", Scope: "team", ID: "flow"}} {
		if err := database.RuntimeSnapshots().Save(ctx, manifest, reference); err != nil {
			t.Fatal(err)
		}
	}
	recordAbandonmentTurn(t, database, "root", "target", id, false)
	if _, err := database.SessionEvents().Append(ctx, "root", session.AnySequence, session.PendingEvent{Type: session.EventToolStarted, CorrelationID: "target", Payload: session.MustJSONPayload(map[string]any{"tool": "shell_exec"})}); err != nil {
		t.Fatal(err)
	}
	if err := database.Checkpoints().Set(ctx, "opaque-target", []byte("original checkpoint evidence")); err != nil {
		t.Fatal(err)
	}
	request := abandonmentRequest("target")
	receipt, err := database.AbandonRecovery(ctx, request)
	if err != nil || receipt.ReleasedReferences != 1 || receipt.EffectsReverted || !reflect.DeepEqual(receipt.TurnIDs, []string{"target"}) {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := database.AbandonRecovery(ctx, request)
	if err != nil || !reflect.DeepEqual(receipt, replayed) {
		t.Fatalf("replay=%+v err=%v", replayed, err)
	}
	request.Reason = "different reason"
	if _, err := database.AbandonRecovery(ctx, request); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("changed request accepted: %v", err)
	}
	refs, err := database.RuntimeSnapshots().References(ctx, id)
	if err != nil || len(refs) != 2 {
		t.Fatalf("shared references lost: %+v %v", refs, err)
	}
	history, err := database.RuntimeSnapshots().ReferenceHistory(ctx, id)
	if err != nil || len(history) != 3 {
		t.Fatalf("history=%+v err=%v", history, err)
	}
	for _, item := range history {
		if item.Reference == ref && (item.ReleasedAt == nil || item.ReleaseReason != receipt.Reason) {
			t.Fatalf("missing release audit: %+v", item)
		}
	}
	if err := database.RuntimeSnapshots().Save(ctx, manifest, ref); !errors.Is(err, tool.ErrSnapshotReferenceReleased) {
		t.Fatalf("released reference reopened: %v", err)
	}
	if _, err := database.RuntimeSnapshots().Load(ctx, id); err != nil {
		t.Fatal("manifest deleted", err)
	}
	checkpoint, found, err := database.Checkpoints().Get(ctx, "opaque-target")
	if err != nil || !found || string(checkpoint) != "original checkpoint evidence" {
		t.Fatalf("checkpoint lost: %s %v", checkpoint, err)
	}
	events, err := database.SessionEvents().LoadByCorrelation(ctx, "root", "target")
	if err != nil || len(events) != 3 || events[1].Type != session.EventToolStarted || events[2].Type != session.EventTurnAbandoned {
		t.Fatalf("evidence overwritten or replay duplicated: %+v %v", events, err)
	}
	if err := session.CheckRecovery(ctx, database.SessionEvents(), "root", "target"); !errors.Is(err, session.ErrRecoveryAbandoned) {
		t.Fatalf("abandoned turn executable: %v", err)
	}
}

func TestAbandonCurrentTurnClosesWholeGoalWithoutDiscardingRounds(t *testing.T) {
	ctx := context.Background()
	database := abandonmentDatabase(t)
	goal := createAbandonmentGoal(t, database)
	manifest := runtimeManifest(t, "v1")
	id, _ := manifest.ID()
	for index, turnID := range []string{"round-1", "round-2"} {
		if _, err := database.Orchestration().StartRound(ctx, goal.ID, index+1, turnID, "input"); err != nil {
			t.Fatal(err)
		}
		if err := database.RuntimeSnapshots().Save(ctx, manifest, tool.SnapshotReference{Kind: "turn", Scope: "root", ID: turnID}); err != nil {
			t.Fatal(err)
		}
		recordAbandonmentTurn(t, database, "root", turnID, id, true)
		if _, err := database.Orchestration().RecordReport(ctx, "root", turnID, agentgoal.Report{Decision: agentgoal.DecisionContinue, Reason: "unfinished"}); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Orchestration().FinishRound(ctx, goal.ID, index+1, agentgoal.Verdict{Decision: agentgoal.DecisionContinue, Reason: "unfinished"}, "durable round candidate"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.AbandonRecovery(ctx, abandonmentRequest("round-1")); err == nil {
		t.Fatal("stale Goal round accepted")
	}
	receipt, err := database.AbandonRecovery(ctx, abandonmentRequest("round-2"))
	if err != nil || receipt.GoalID != goal.ID || receipt.ReleasedReferences != 2 || len(receipt.TurnIDs) != 2 {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	stored, err := database.Orchestration().GetGoal(ctx, goal.ID)
	if err != nil || stored.State != agentgoal.StateCanceled || stored.StopReason != agentgoal.StopAbandoned || stored.Result != "durable round candidate" || stored.Decision != "" {
		t.Fatalf("goal=%+v err=%v", stored, err)
	}
	for _, turnID := range receipt.TurnIDs {
		_, round, found, err := database.Orchestration().GoalForTurn(ctx, "root", turnID)
		if err != nil || !found || round.Candidate != "durable round candidate" || round.CompletedAt == nil {
			t.Fatalf("round lost: %+v %v", round, err)
		}
		if _, found, err := database.Orchestration().ReportForTurn(ctx, "root", turnID); err != nil || !found {
			t.Fatalf("report lost: %v", err)
		}
		if err := session.CheckRecovery(ctx, database.SessionEvents(), "root", turnID); !errors.Is(err, session.ErrRecoveryAbandoned) {
			t.Fatalf("old round accepted: %v", err)
		}
	}
	if refs, err := database.RuntimeSnapshots().References(ctx, id); err != nil || len(refs) != 0 {
		t.Fatalf("Goal refs retained: %+v %v", refs, err)
	}
	if _, err := database.AbandonRecovery(ctx, abandonmentRequest("round-1")); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("stale round replay accepted: %v", err)
	}
}

func TestAbandonLegacyAndUnstartedRecovery(t *testing.T) {
	for _, mode := range []string{"legacy-turn", "unstarted-reference", "orphan-goal", "unstarted-goal-round"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			database := abandonmentDatabase(t)
			manifest := runtimeManifest(t, "v1")
			request := abandonmentRequest("target")
			wantReleased := 0
			switch mode {
			case "legacy-turn":
				recordAbandonmentTurn(t, database, "root", "target", "", false)
			case "unstarted-reference":
				if err := database.RuntimeSnapshots().Save(ctx, manifest, tool.SnapshotReference{Kind: "turn", Scope: "root", ID: "target"}); err != nil {
					t.Fatal(err)
				}
				wantReleased = 1
			default:
				goal := createAbandonmentGoal(t, database)
				request.TurnID, request.GoalID = "", goal.ID
				if mode == "unstarted-goal-round" {
					if _, err := database.Orchestration().StartRound(ctx, goal.ID, 1, "target", "input"); err != nil {
						t.Fatal(err)
					}
				}
			}
			receipt, err := database.AbandonRecovery(ctx, request)
			if err != nil || receipt.ReleasedReferences != wantReleased {
				t.Fatalf("receipt=%+v err=%v", receipt, err)
			}
			if mode == "orphan-goal" {
				if len(receipt.TurnIDs) != 0 {
					t.Fatal("invented a turn for orphan Goal")
				}
			} else if err := database.RuntimeSnapshots().Save(ctx, manifest, tool.SnapshotReference{Kind: "turn", Scope: "root", ID: "target"}); !errors.Is(err, tool.ErrSnapshotReferenceReleased) {
				t.Fatalf("resurrected abandoned consumer: %v", err)
			}
		})
	}
}

func TestAbandonRecoveryRollsBackAllLifecycleWrites(t *testing.T) {
	for _, failure := range []string{"audit", "receipt", "corrupt-event", "snapshot-mismatch"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			database := abandonmentDatabase(t)
			goal := createAbandonmentGoal(t, database)
			if _, err := database.Orchestration().StartRound(ctx, goal.ID, 1, "target", "input"); err != nil {
				t.Fatal(err)
			}
			manifest := runtimeManifest(t, "v1")
			id, _ := manifest.ID()
			if err := database.RuntimeSnapshots().Save(ctx, manifest, tool.SnapshotReference{Kind: "turn", Scope: "root", ID: "target"}); err != nil {
				t.Fatal(err)
			}
			eventID := id
			if failure == "snapshot-mismatch" {
				eventID = "different snapshot"
			}
			recordAbandonmentTurn(t, database, "root", "target", eventID, false)
			var statement string
			switch failure {
			case "audit":
				statement = `CREATE TRIGGER fail_abandon BEFORE INSERT ON session_events WHEN NEW.event_type = 'turn.abandoned' BEGIN SELECT RAISE(ABORT, 'audit failed'); END`
			case "receipt":
				statement = `CREATE TRIGGER fail_abandon BEFORE INSERT ON recovery_abandonments BEGIN SELECT RAISE(ABORT, 'receipt failed'); END`
			case "corrupt-event":
				statement = `UPDATE session_events SET checksum = 'corrupted' WHERE correlation_id = 'target'`
			}
			if statement != "" {
				if _, err := database.db.Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := database.AbandonRecovery(ctx, abandonmentRequest("target")); err == nil {
				t.Fatal("failure ignored")
			}
			stored, err := database.Orchestration().GetGoal(ctx, goal.ID)
			if err != nil || stored.State != agentgoal.StateRunning || stored.StopReason != "" {
				t.Fatalf("Goal changed on failure: %+v %v", stored, err)
			}
			if refs, err := database.RuntimeSnapshots().References(ctx, id); err != nil || len(refs) != 1 {
				t.Fatalf("reference lost on failure: %+v %v", refs, err)
			}
			var count int
			if err := database.db.QueryRow(`SELECT (SELECT COUNT(*) FROM recovery_abandonments) + (SELECT COUNT(*) FROM session_events WHERE event_type IN ('turn.abandoned', 'goal.abandoned'))`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("partial audit persisted: %d %v", count, err)
			}
		})
	}
}

func TestAbandonRecoveryRejectsDependentOrDelegatedWork(t *testing.T) {
	for _, dependency := range []string{"child", "task", "flow", "child-session"} {
		t.Run(dependency, func(t *testing.T) {
			ctx := context.Background()
			database := abandonmentDatabase(t)
			root, _, err := database.Orchestration().EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "root"})
			if err != nil {
				t.Fatal(err)
			}
			request := abandonmentRequest("target")
			switch dependency {
			case "child", "child-session":
				child, err := database.Orchestration().CreateChild(ctx, orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker-v1"})
				if err != nil {
					t.Fatal(err)
				}
				if dependency == "child-session" {
					request.SessionID = child.SessionID
				}
			case "task":
				if _, err := database.Orchestration().CreateTask(ctx, orchestration.TaskSpec{TeamID: root.TeamID, Title: "pending"}); err != nil {
					t.Fatal(err)
				}
			case "flow":
				input, _ := agentflow.NewValue("task")
				id, _ := agentflow.NewID()
				snapshotID, _ := tool.NewRegistry().Snapshot().ID()
				if _, err := database.Orchestration().CreateRun(ctx, agentflow.RunRequest{ID: id, IdempotencyKey: "key", Definition: "test", DefinitionRevision: "v1", DefinitionFingerprint: strings.Repeat("a", 64), SessionID: root.SessionID, TeamID: root.TeamID, AgentID: root.AgentID, Input: input, RegistryGeneration: 1, RuntimeSnapshotID: snapshotID}); err != nil {
					t.Fatal(err)
				}
			}
			recordAbandonmentTurn(t, database, request.SessionID, "target", "", false)
			before, _ := database.SessionEvents().CurrentSequence(ctx, request.SessionID)
			_, err = database.AbandonRecovery(ctx, request)
			if err == nil || (dependency != "child-session" && !errors.Is(err, session.ErrRecoveryDependency)) {
				t.Fatalf("dependency accepted: %v", err)
			}
			after, _ := database.SessionEvents().CurrentSequence(ctx, request.SessionID)
			if after != before {
				t.Fatal("rejected abandonment changed events")
			}
		})
	}
}

func TestAbandonRecoveryRequiresExistingUnfinishedTargetAndConsent(t *testing.T) {
	ctx := context.Background()
	database := abandonmentDatabase(t)
	recordAbandonmentTurn(t, database, "root", "completed", "", true)
	recordAbandonmentTurn(t, database, "root", "target", "", false)
	for _, request := range []session.AbandonRequest{
		abandonmentRequest("missing"), abandonmentRequest("completed"),
		{SessionID: "other", TurnID: "target", EffectsReviewed: true, Reason: "reviewed"},
		{SessionID: "root", TurnID: "target", Reason: "not confirmed"},
		{SessionID: "root", TurnID: "target", EffectsReviewed: true},
		{SessionID: "root", TurnID: "target", GoalID: "goal", EffectsReviewed: true, Reason: "ambiguous"},
	} {
		if _, err := database.AbandonRecovery(ctx, request); err == nil {
			t.Fatalf("accepted %+v", request)
		}
	}
	if err := session.CheckRecovery(ctx, database.SessionEvents(), "root", "target"); err != nil {
		t.Fatal("validation failure disabled recovery", err)
	}
}

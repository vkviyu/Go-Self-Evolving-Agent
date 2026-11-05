package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

func TestAdmissionIsAtomicIdempotentAndKeepsOriginalClock(t *testing.T) {
	database, err := Open(context.Background(), filepath.Join(t.TempDir(), "goals.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := database.Orchestration()
	contract := &agentgoal.Contract{Objective: "verify all entries", Scope: "read-only", Criteria: []agentgoal.Criterion{{Description: "complete coverage", Kind: "semantic"}}}
	started := time.Now().UTC().Add(-time.Second)
	request := agentgoal.CreateRequest{ID: "admitted", SessionID: "test", Text: "original user request", Contract: contract, Assessment: agentgoal.AssessmentAdaptive, MaxRounds: 3, MaxWallTime: time.Minute, StartedAt: started}
	first, round, err := store.AdmitGoal(context.Background(), request, "turn")
	if err != nil || round.Number != 1 {
		t.Fatalf("first=%+v round=%+v err=%v", first, round, err)
	}
	request.StartedAt = time.Now().UTC()
	replayed, _, err := store.AdmitGoal(context.Background(), request, "turn")
	if err != nil || replayed.Version != first.Version || !replayed.StartedAt.Equal(started) {
		t.Fatalf("replayed=%+v err=%v", replayed, err)
	}
	request.ID = "orphan"
	if _, _, err := store.AdmitGoal(context.Background(), request, "turn"); err == nil {
		t.Fatal("same turn admitted to a second task")
	}
	if _, err := store.GetGoal(context.Background(), "orphan"); !errors.Is(err, orchestration.ErrNotFound) {
		t.Fatalf("failed admission left an orphan: %v", err)
	}
	loaded, _, found, err := store.GoalForTurn(context.Background(), "test", "turn")
	if err != nil || !found || loaded.Contract.Scope != "read-only" || loaded.Text != "original user request" {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
}

func TestGoalContractMigrationPreservesVersion24Goals(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	dsn, err := dataSourceName(path)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	if _, err := legacy.Exec(`CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, checksum TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, item := range migrations {
		if item.version > 24 {
			break
		}
		if _, err := legacy.Exec(item.ddl); err != nil {
			t.Fatal(err)
		}
		if _, err := legacy.Exec(`INSERT INTO schema_migrations VALUES(?,?,?)`, item.version, migrationChecksum(item.ddl), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := legacy.Exec(`INSERT INTO goal_runs(goal_id,session_id,goal_text,state,assessment,max_rounds,max_wall_time_ns,version,started_at,updated_at) VALUES('old','session','original','running','self',3,?,1,?,?)`, time.Minute.Nanoseconds(), now, now); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	database, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	goal, err := database.Orchestration().GetGoal(ctx, "old")
	if err != nil || goal.Contract != nil || goal.Text != "original" || goal.Assessment != agentgoal.AssessmentSelf {
		t.Fatalf("migrated=%+v err=%v", goal, err)
	}
}

func TestGoalStoreEnforcesIdempotentRequests(t *testing.T) {
	t.Parallel()
	database, err := Open(context.Background(), filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: session.ID("goal-store-session")})
	if err != nil {
		t.Fatal(err)
	}
	request := agentgoal.CreateRequest{
		ID: "goal-store-test", SessionID: root.SessionID, TeamID: root.TeamID, AgentID: root.AgentID,
		Text: "complete the task", Assessment: agentgoal.AssessmentSelf, MaxRounds: 3, MaxWallTime: time.Minute,
	}
	first, err := store.CreateGoal(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateGoal(context.Background(), request)
	if err != nil || second.ID != first.ID || second.Version != first.Version {
		t.Fatalf("idempotent goal = %#v err=%v", second, err)
	}
	conflict := request
	conflict.Text = "different task"
	if _, err := store.CreateGoal(context.Background(), conflict); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("goal conflict error = %v", err)
	}
	round, err := store.StartRound(context.Background(), first.ID, 1, "turn-store-1", request.Text)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := store.StartRound(context.Background(), first.ID, 1, "turn-store-1", request.Text)
	if err != nil || replayed.TurnID != round.TurnID {
		t.Fatalf("round replay = %#v err=%v", replayed, err)
	}
	if _, err := store.StartRound(context.Background(), first.ID, 1, "turn-store-1", "different input"); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("round conflict error = %v", err)
	}
	report := agentgoal.Report{Decision: agentgoal.DecisionContinue, Reason: "more work"}
	if _, err := store.RecordReport(context.Background(), root.SessionID, round.TurnID, report); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordReport(context.Background(), root.SessionID, round.TurnID, report); err != nil {
		t.Fatal(err)
	}
	report.Reason = "different"
	if _, err := store.RecordReport(context.Background(), root.SessionID, round.TurnID, report); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("report conflict error = %v", err)
	}
}

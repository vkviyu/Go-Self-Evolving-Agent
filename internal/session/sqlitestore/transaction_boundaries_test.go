package sqlitestore

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

// Snapshot every persisted row, including audit, identity and sequence tables.
// Comparing only the main projection would miss a partially committed helper.
func snapshotSQLiteRows(t *testing.T, ctx context.Context, query readQuery) string {
	t.Helper()
	rows, err := query.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	if err := closeRetentionRows(rows); err != nil {
		t.Fatal(err)
	}
	snapshot := make(map[string][]string)
	for _, table := range tables {
		rows, err := query.QueryContext(ctx, `SELECT * FROM "`+strings.ReplaceAll(table, `"`, `""`)+`"`)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		snapshot[table] = nil
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for index := range values {
				pointers[index] = &values[index]
			}
			if err := rows.Scan(pointers...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			raw, err := json.Marshal(values)
			if err != nil {
				rows.Close()
				t.Fatal(err)
			}
			snapshot[table] = append(snapshot[table], string(raw))
		}
		if err := closeRetentionRows(rows); err != nil {
			t.Fatal(err)
		}
		sort.Strings(snapshot[table])
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestOrchestrationTransactionBoundaries(t *testing.T) {
	for _, operation := range []string{"root", "spawn-with-claim", "agent-transition", "dependency", "completion-unblocks", "claim-reaper"} {
		t.Run(operation, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			database := abandonmentDatabase(t)
			store := database.Orchestration()
			seed := orchestration.RootThreadSeed{SessionID: "atomic-root"}
			var run func() error
			condition := "NEW.event_type='thread.created'"
			if operation == "root" {
				run = func() error { _, _, err := store.EnsureRootThread(ctx, seed); return err }
			} else {
				root, _, err := store.EnsureRootThread(ctx, seed)
				if err != nil {
					t.Fatal(err)
				}
				task, err := store.CreateTask(ctx, orchestration.TaskSpec{TeamID: root.TeamID, Title: "prerequisite"})
				if err != nil {
					t.Fatal(err)
				}
				task, err = store.TransitionTask(ctx, orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskReady})
				if err != nil {
					t.Fatal(err)
				}
				switch operation {
				case "spawn-with-claim":
					spec := orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker-v1", TaskID: task.ID, IdempotencyKey: "atomic-spawn"}
					run = func() error { _, err := store.CreateChild(ctx, spec); return err }
				case "agent-transition":
					condition = "NEW.event_type='agent.run_started'"
					run = func() error {
						_, err := store.TransitionAgent(ctx, orchestration.AgentTransition{AgentID: root.AgentID, Expected: 1, Target: orchestration.AgentRunning})
						return err
					}
				case "dependency", "completion-unblocks":
					dependent, err := store.CreateTask(ctx, orchestration.TaskSpec{TeamID: root.TeamID, Title: "dependent"})
					if err != nil {
						t.Fatal(err)
					}
					condition = fmt.Sprintf("NEW.event_type='task.updated' AND NEW.stream_id='%s'", dependent.ID)
					if operation == "dependency" {
						run = func() error { return store.AddDependency(ctx, dependent.ID, task.ID, root.AgentID, "add") }
						break
					}
					if err := store.AddDependency(ctx, dependent.ID, task.ID, root.AgentID, "add"); err != nil {
						t.Fatal(err)
					}
					task, err = store.ClaimTask(ctx, task.ID, root.AgentID, task.Version)
					if err != nil {
						t.Fatal(err)
					}
					task, err = store.TransitionTask(ctx, orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskInProgress, ActorAgentID: root.AgentID})
					if err != nil {
						t.Fatal(err)
					}
					run = func() error {
						_, err := store.TransitionTask(ctx, orchestration.TaskTransition{TaskID: task.ID, Expected: task.Version, Target: orchestration.TaskCompleted, ActorAgentID: root.AgentID})
						return err
					}
				case "claim-reaper":
					if _, err := store.ClaimTask(ctx, task.ID, root.AgentID, task.Version); err != nil {
						t.Fatal(err)
					}
					condition = "NEW.event_type='task.claim_expired'"
					run = func() error { _, err := store.ReapExpiredTaskClaims(ctx, time.Now().Add(time.Hour)); return err }
				}
			}
			if _, err := database.db.ExecContext(ctx, `CREATE TRIGGER fail_boundary BEFORE INSERT ON orchestration_events WHEN `+condition+` BEGIN SELECT RAISE(ABORT,'injected audit failure'); END`); err != nil {
				t.Fatal(err)
			}
			before := snapshotSQLiteRows(t, ctx, database.db)
			if err := run(); err == nil || !strings.Contains(err.Error(), "injected audit failure") {
				t.Fatalf("did not reach injected failure: %v", err)
			}
			if after := snapshotSQLiteRows(t, ctx, database.db); before != after {
				t.Fatal("failed operation left partial projections, identity or audit rows")
			}
			if _, err := database.db.ExecContext(ctx, `DROP TRIGGER fail_boundary`); err != nil {
				t.Fatal(err)
			}
			if err := run(); err != nil {
				t.Fatalf("retry after rollback failed: %v", err)
			}
			after := snapshotSQLiteRows(t, ctx, database.db)
			if before == after {
				t.Fatal("successful retry made no durable changes")
			}
			if operation == "root" || operation == "spawn-with-claim" {
				if err := run(); err != nil {
					t.Fatal(err)
				}
				if replay := snapshotSQLiteRows(t, ctx, database.db); replay != after {
					t.Fatal("idempotent replay duplicated durable changes")
				}
			}
		})
	}
}

func TestMaintenanceAuditUsesReadOnlyTransactionView(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	database := abandonmentDatabase(t)
	source := []byte("offline audit fixture")
	if err := database.Plugins().Save(ctx, catalogRecord("v1", source), source); err != nil {
		t.Fatal(err)
	}
	manifest := runtimeManifest(t, "v1")
	id, err := manifest.ID()
	if err != nil {
		t.Fatal(err)
	}
	ref := tool.SnapshotReference{Kind: "turn", Scope: "root", ID: "done"}
	if err := database.RuntimeSnapshots().Save(ctx, manifest, ref); err != nil {
		t.Fatal(err)
	}
	recordAbandonmentTurn(t, database, "root", "done", id, true)
	before := snapshotSQLiteRows(t, ctx, database.db)
	baselineRetention, err := database.RetentionPlan(ctx, "root")
	if err != nil || len(baselineRetention.Consumers) != 1 {
		t.Fatalf("retention=%+v err=%v", baselineRetention, err)
	}
	baselineCollection, err := database.PluginCollectionPlan(ctx, "echo", "v1")
	if err != nil || len(baselineCollection.Blockers) == 0 {
		t.Fatalf("collection=%+v err=%v", baselineCollection, err)
	}
	if snapshotSQLiteRows(t, ctx, database.db) != before {
		t.Fatal("public previews changed persistent state")
	}
	tx, err := database.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO runtime_consumer_closures VALUES('turn','root','done','test-plan','timestamp')`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runtime_snapshot_refs SET released_at='timestamp' WHERE kind='turn' AND scope='root' AND consumer_id='done'`); err != nil {
		t.Fatal(err)
	}
	// Passing only read methods prevents the audit from obtaining a mutation or
	// lifecycle method. Querying the pool instead would deadlock its one connection.
	view := struct{ readQuery }{tx}
	staged := snapshotSQLiteRows(t, ctx, view)
	plan, err := planRetention(ctx, view, "root")
	if err != nil || len(plan.Consumers) != 0 || len(plan.Blockers) != 0 {
		t.Fatalf("audit did not see staged closure: %+v %v", plan, err)
	}
	collection, err := planPluginCollection(ctx, view, "echo", "v1")
	if err != nil || len(collection.Blockers) != 0 {
		t.Fatalf("collection did not see staged release: %+v %v", collection, err)
	}
	if snapshotSQLiteRows(t, ctx, view) != staged {
		t.Fatal("internal audit changed transaction state")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	reopenedPlan, err := database.RetentionPlan(ctx, session.ID("root"))
	if err != nil || !reflect.DeepEqual(reopenedPlan, baselineRetention) {
		t.Fatalf("preview leaked staged state: %+v %v", reopenedPlan, err)
	}
	reopenedCollection, err := database.PluginCollectionPlan(ctx, "echo", "v1")
	if err != nil || !reflect.DeepEqual(reopenedCollection, baselineCollection) {
		t.Fatalf("collection leaked staged state: %+v %v", reopenedCollection, err)
	}
	if snapshotSQLiteRows(t, ctx, database.db) != before {
		t.Fatal("rollback/preview changed durable state")
	}
}

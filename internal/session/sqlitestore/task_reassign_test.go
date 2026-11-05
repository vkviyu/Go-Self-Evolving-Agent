package sqlitestore

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

type failedTaskFixture struct {
	database *Database
	store    *OrchestrationStore
	root     orchestration.RootThread
	task     orchestration.TaskSnapshot
	agent    orchestration.AgentSnapshot
}

func newFailedTaskFixture(t *testing.T, toolEvents []session.PendingEvent, beforeFailure func(*failedTaskFixture)) *failedTaskFixture {
	t.Helper()
	ctx := context.Background()
	database, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	f := &failedTaskFixture{database: database, store: database.Orchestration()}
	f.root, _, err = f.store.EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "reassign-root"})
	if err != nil {
		t.Fatal(err)
	}
	f.task, err = f.store.CreateTask(ctx, orchestration.TaskSpec{TeamID: f.root.TeamID, Title: "inspect code"})
	if err != nil {
		t.Fatal(err)
	}
	f.task, err = f.store.TransitionTask(ctx, orchestration.TaskTransition{TaskID: f.task.ID, Expected: f.task.Version, Target: orchestration.TaskReady})
	if err != nil {
		t.Fatal(err)
	}
	f.agent, err = f.store.CreateChild(ctx, orchestration.SpawnSpec{TeamID: f.root.TeamID, ParentAgentID: f.root.AgentID, TaskID: f.task.ID, ProfileRevision: "worker-v1", InitialPrompt: "inspect"})
	if err != nil {
		t.Fatal(err)
	}
	runID, _ := orchestration.NewRunID()
	f.agent, err = f.store.TransitionAgent(ctx, orchestration.AgentTransition{AgentID: f.agent.ID, Expected: f.agent.Version, Target: orchestration.AgentRunning, RunID: runID})
	if err != nil {
		t.Fatal(err)
	}
	f.task, err = f.store.GetTask(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.task, err = f.store.TransitionTask(ctx, orchestration.TaskTransition{TaskID: f.task.ID, Expected: f.task.Version, Target: orchestration.TaskInProgress, AssigneeAgentID: f.agent.ID, ActorAgentID: f.agent.ID, CorrelationID: string(runID)})
	if err != nil {
		t.Fatal(err)
	}
	if beforeFailure != nil {
		beforeFailure(f)
	}
	events := []session.PendingEvent{{Type: session.EventTurnStarted, CorrelationID: "turn", Payload: session.MustJSONPayload(map[string]any{"turn_id": "turn", "run_id": runID})}}
	events = append(events, toolEvents...)
	events = append(events, session.PendingEvent{Type: session.EventTurnFailed, CorrelationID: "turn", Payload: json.RawMessage(`{}`)})
	if _, err := database.SessionEvents().Append(ctx, f.agent.SessionID, session.AnySequence, events...); err != nil {
		t.Fatal(err)
	}
	f.task, err = f.store.GetTask(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.task, err = f.store.TransitionTask(ctx, orchestration.TaskTransition{TaskID: f.task.ID, Expected: f.task.Version, Target: orchestration.TaskFailed, AssigneeAgentID: f.agent.ID, ActorAgentID: f.agent.ID, CorrelationID: string(runID)})
	if err != nil {
		t.Fatal(err)
	}
	result := orchestration.AgentResult{Status: orchestration.ResultFailed, Error: "iteration limit", Retryable: true, RetryCode: orchestration.TaskAttemptIterationLimit}
	f.agent, err = f.store.TransitionAgent(ctx, orchestration.AgentTransition{AgentID: f.agent.ID, Expected: f.agent.Version, Target: orchestration.AgentFailed, RunID: runID, Result: &result})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func retryCallEvent(kind session.EventType, turn, risk string) session.PendingEvent {
	return session.PendingEvent{Type: kind, CorrelationID: "call", Payload: session.MustJSONPayload(map[string]any{"turn_id": turn, "call_id": "call", "tool": "inspect", "revision": "v1", "risk_class": risk, "result": map[string]any{"ok": true}})}
}

func TestReassignTaskReadOnlyProofAndIdempotency(t *testing.T) {
	f := newFailedTaskFixture(t, []session.PendingEvent{retryCallEvent(session.EventToolProposed, "turn", "read"), retryCallEvent(session.EventToolStarted, "turn", ""), retryCallEvent(session.EventToolSucceeded, "turn", "")}, nil)
	ctx := context.Background()
	ready, err := f.store.ReassignTask(ctx, f.task.ID, f.agent.ID, f.task.Version, "new attempt")
	if err != nil {
		t.Fatal(err)
	}
	if ready.ID != f.task.ID || ready.State != orchestration.TaskReady || ready.Attempt != 1 || ready.AssigneeAgentID != "" || ready.ClaimExpiresAt != nil {
		t.Fatalf("bad new attempt: %#v", ready)
	}
	old, err := f.store.GetAgent(ctx, f.agent.ID)
	if err != nil || old.State != orchestration.AgentFailed || old.CurrentRunID != f.agent.CurrentRunID {
		t.Fatalf("old history changed: %#v %v", old, err)
	}
	// A new owner may already be executing before the caller retries its lost
	// response. Replaying the old request must not increment or steal its claim.
	newAgent, err := f.store.CreateChild(ctx, orchestration.SpawnSpec{TeamID: f.root.TeamID, ParentAgentID: f.root.AgentID, TaskID: f.task.ID, ProfileRevision: "worker-v1"})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := f.store.ReassignTask(ctx, f.task.ID, f.agent.ID, f.task.Version, "different diagnostic")
	if err != nil {
		t.Fatal(err)
	}
	if replay.Attempt != 1 || replay.AssigneeAgentID != newAgent.ID || replay.State != orchestration.TaskClaimed {
		t.Fatalf("idempotent replay changed new assignment: %#v", replay)
	}
	var count int
	if err := f.database.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM orchestration_events WHERE stream_id = ? AND event_type = ?`, string(f.task.ID), string(orchestration.EventTaskReassigned)).Scan(&count); err != nil || count != 1 {
		t.Fatalf("reassignment count=%d err=%v", count, err)
	}
}

func TestReassignTaskRejectsUnprovenOrEffectfulCalls(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []session.PendingEvent
	}{
		{"successful write", []session.PendingEvent{retryCallEvent(session.EventToolProposed, "turn", "write"), retryCallEvent(session.EventToolStarted, "turn", ""), retryCallEvent(session.EventToolSucceeded, "turn", "")}},
		{"failed read without success", []session.PendingEvent{retryCallEvent(session.EventToolProposed, "turn", "read"), retryCallEvent(session.EventToolStarted, "turn", ""), retryCallEvent(session.EventToolFailed, "turn", "")}},
		{"missing proposal", []session.PendingEvent{retryCallEvent(session.EventToolStarted, "turn", ""), retryCallEvent(session.EventToolSucceeded, "turn", "")}},
		{"wrong turn receipt", []session.PendingEvent{retryCallEvent(session.EventToolProposed, "turn", "read"), retryCallEvent(session.EventToolStarted, "turn", ""), retryCallEvent(session.EventToolSucceeded, "another-turn", "")}},
		{"unknown outcome", []session.PendingEvent{retryCallEvent(session.EventToolOutcomeUnknown, "turn", "")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFailedTaskFixture(t, tc.events, nil)
			if _, err := f.store.ReassignTask(context.Background(), f.task.ID, f.agent.ID, f.task.Version, "retry"); !errors.Is(err, orchestration.ErrTaskReassignUnsafe) {
				t.Fatalf("got %v", err)
			}
			current, _ := f.store.GetTask(context.Background(), f.task.ID)
			if current.Attempt != 0 || current.State != orchestration.TaskFailed {
				t.Fatalf("unsafe attempt reopened: %#v", current)
			}
		})
	}
}

func TestReassignTaskSafetyGates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, *failedTaskFixture)
		want   error
	}{
		{"attempt limit", func(t *testing.T, f *failedTaskFixture) {
			_, err := f.database.db.Exec(`UPDATE tasks SET attempt = 2 WHERE task_id = ?`, string(f.task.ID))
			if err != nil {
				t.Fatal(err)
			}
		}, orchestration.ErrTaskAttemptLimit},
		{"missing trusted code", func(t *testing.T, f *failedTaskFixture) {
			result := *f.agent.Result
			result.RetryCode = ""
			data, _ := json.Marshal(result)
			_, err := f.database.db.Exec(`UPDATE agents SET result = ? WHERE agent_id = ?`, data, string(f.agent.ID))
			if err != nil {
				t.Fatal(err)
			}
		}, orchestration.ErrTaskReassignUnsafe},
		{"team canceled", func(t *testing.T, f *failedTaskFixture) {
			_, err := f.store.TransitionTeam(context.Background(), orchestration.TeamTransition{TeamID: f.root.TeamID, Expected: orchestration.AnySequence, Target: orchestration.TeamCanceling})
			if err != nil {
				t.Fatal(err)
			}
		}, orchestration.ErrTeamNotActive},
		{"run identity changed", func(t *testing.T, f *failedTaskFixture) {
			runID, _ := orchestration.NewRunID()
			_, err := f.database.db.Exec(`UPDATE agents SET current_run_id = ? WHERE agent_id = ?`, string(runID), string(f.agent.ID))
			if err != nil {
				t.Fatal(err)
			}
		}, orchestration.ErrTaskReassignUnsafe},
		{"expired lease is not process exit", func(t *testing.T, f *failedTaskFixture) {
			_, err := f.database.db.Exec(`INSERT INTO workspace_leases(lease_id,team_id,agent_id,path,mode,state,expires_at,created_at,updated_at) VALUES('lease-test',?,?,'.','write','expired','','','')`, string(f.root.TeamID), string(f.agent.ID))
			if err != nil {
				t.Fatal(err)
			}
		}, orchestration.ErrTaskReassignUnsafe},
		{"active view", func(t *testing.T, f *failedTaskFixture) { insertRetryView(t, f, "active") }, orchestration.ErrTaskReassignUnsafe},
		{"merge delivery", func(t *testing.T, f *failedTaskFixture) {
			insertRetryView(t, f, "released")
			_, err := f.database.db.Exec(`INSERT INTO merge_queue(merge_id,team_id,agent_id,task_id,view_id,base_revision,patch_artifact,state,version,created_at,updated_at) VALUES('merge-test',?,?,?,'view-test','','','pending',1,'','')`, string(f.root.TeamID), string(f.agent.ID), string(f.task.ID))
			if err != nil {
				t.Fatal(err)
			}
		}, orchestration.ErrTaskReassignUnsafe},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFailedTaskFixture(t, nil, nil)
			tc.mutate(t, f)
			if _, err := f.store.ReassignTask(context.Background(), f.task.ID, f.agent.ID, f.task.Version, "retry"); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
	t.Run("live descendant", func(t *testing.T) {
		f := newFailedTaskFixture(t, nil, func(f *failedTaskFixture) {
			_, err := f.store.CreateChild(context.Background(), orchestration.SpawnSpec{TeamID: f.root.TeamID, ParentAgentID: f.agent.ID, ProfileRevision: "worker-v1"})
			if err != nil {
				t.Fatal(err)
			}
		})
		if _, err := f.store.ReassignTask(context.Background(), f.task.ID, f.agent.ID, f.task.Version, "retry"); !errors.Is(err, orchestration.ErrTaskReassignUnsafe) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("stale CAS", func(t *testing.T) {
		f := newFailedTaskFixture(t, nil, nil)
		if _, err := f.store.ReassignTask(context.Background(), f.task.ID, f.agent.ID, f.task.Version-1, "retry"); !errors.Is(err, orchestration.ErrTaskStateConflict) {
			t.Fatalf("got %v", err)
		}
	})
}

func insertRetryView(t *testing.T, f *failedTaskFixture, state string) {
	t.Helper()
	_, err := f.database.db.Exec(`INSERT INTO workspace_views(view_id,team_id,agent_id,task_id,mode,workspace_root,root_path,state,created_at,updated_at) VALUES('view-test',?,?,?,'snapshot','/tmp','/tmp/retry-view',?,'','')`, string(f.root.TeamID), string(f.agent.ID), string(f.task.ID), state)
	if err != nil {
		t.Fatal(err)
	}
}

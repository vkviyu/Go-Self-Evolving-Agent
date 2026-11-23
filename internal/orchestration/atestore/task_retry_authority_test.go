package atestore

import (
	"errors"
	"testing"

	"github.com/vkviyu/agent-temporal-engine/agentprofile"
	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

func TestTaskRetryUsesNativeSessionFence(t *testing.T) {
	store := newTestStore(t)
	ctx := t.Context()
	team, owner := taskNativeSeed(t, store)
	task := taskNativeCreate(t, store, team.ID, "native-retry")
	owner.AssignedTaskID, owner.SessionID, owner.CurrentRunID, owner.State = task.ID, "native-retry-session", "run", orchestration.AgentFailed
	owner.Result = &orchestration.AgentResult{Status: orchestration.ResultFailed, Retryable: true, RetryCode: orchestration.TaskAttemptIterationLimit}
	task.State, task.AssigneeAgentID = orchestration.TaskFailed, owner.ID
	err := store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		if err := putRecord(tx, "agent", string(owner.ID), owner); err != nil {
			return err
		}
		if err := putRecord(tx, "task", string(task.ID), task); err != nil {
			return err
		}
		return taskEvent(tx, task, orchestration.PendingEvent{
			Type: orchestration.EventTaskUpdated, CorrelationID: "run",
			Payload: orchestration.MustJSONPayload(map[string]any{"to": orchestration.TaskFailed, "assignee_agent_id": owner.ID, "attempt": 0}),
		})
	})
	coreMust(t, err)
	for _, event := range taskProofEvents("read") {
		_, err := store.sessions.Append(ctx, owner.SessionID, session.AnySequence, session.PendingEvent{
			Type: event.Type, CorrelationID: event.CorrelationID, Payload: event.Payload,
		})
		coreMust(t, err)
	}
	ready, err := store.ReassignTask(ctx, task.ID, owner.ID, task.Version, "retry read-only attempt")
	if err != nil || ready.State != orchestration.TaskReady || ready.Attempt != 1 || ready.AssigneeAgentID != "" {
		t.Fatalf("native retry=%+v err=%v", ready, err)
	}
}

func TestNativeSessionFenceRejectsAppendAfterProof(t *testing.T) {
	fixture := newCoreFixture(t)
	ctx := t.Context()
	store := fixture.store
	_, err := store.sessions.Append(ctx, "fence-race", 0, session.PendingEvent{
		Type: session.EventSessionCreated, Payload: session.MustJSONPayload(map[string]any{}),
	})
	coreMust(t, err)
	snapshot, err := fixture.process.engine.Snapshot(ctx)
	coreMust(t, err)
	changed := false
	err = store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		if err := tx.RequireSessionSequence(snapshot.Fence.Store, "sessions", "fence-race", 1); err != nil {
			return err
		}
		if !changed {
			changed = true
			// Advance only the native agent profile after checking its history.
			// The host-state root remains unchanged, so its CAS is insufficient.
			ack, err := fixture.process.engine.ApplyAgent(ctx, api.ApplyAgentRequest{
				Key: model.RequestKey{Space: "sessions", Principal: "gsea", RequestID: "append-after-proof"},
				Command: agentprofile.Command{AppendSessionEvents: &agentprofile.AppendSessionEventsCommand{
					Session: "fence-race", ExpectedSequence: 1,
					Events: []agentprofile.HostEvent{{Kind: agentprofile.HostEventHost, Schema: "test/race", SchemaVersion: 1, Encoding: "application/json", Payload: []byte(`{}`)}},
				}},
			})
			if err != nil {
				return err
			}
			if ack.Code != api.CodeDurableAck {
				t.Fatalf("append failed: %+v", ack)
			}
		}
		return tx.Put("unsafe-reassignment", true)
	})
	if !errors.Is(err, ateauthority.ErrConflict) {
		t.Fatalf("stale native history was accepted: %v", err)
	}
	coreMust(t, store.authority.View(ctx, func(tx *ateauthority.Tx) error {
		var value bool
		found, err := tx.Get("unsafe-reassignment", &value)
		if found {
			t.Error("reassignment committed after the session advanced")
		}
		return err
	}))
}

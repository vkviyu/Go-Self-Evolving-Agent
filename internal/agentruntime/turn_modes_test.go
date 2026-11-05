package agentruntime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
)

func TestCheckpointAdmissionPolicyIsRestoredNotReclassified(t *testing.T) {
	for _, tc := range []struct {
		payload string
		want    bool
	}{
		{`{"goal_mode":true,"goal_activation":"auto"}`, true},
		{`{"goal_mode":true,"goal_activation":"always"}`, false},
		{`{"goal_mode":true}`, false},
		{`{"goal_mode":false,"goal_activation":"auto"}`, false},
	} {
		store := memorystore.New()
		_, err := store.Append(context.Background(), "session", session.AnySequence, session.PendingEvent{Type: session.EventTurnStarted, CorrelationID: "turn", Payload: json.RawMessage(tc.payload)})
		if err != nil {
			t.Fatal(err)
		}
		factory := &Factory{events: store, goalMode: agentgoal.Config{Enabled: false, Activation: agentgoal.ActivationAlways}}
		auto, err := factory.AutoAdmissionForTurn(context.Background(), "session", "turn")
		if err != nil || auto != tc.want {
			t.Fatalf("payload=%s auto=%v err=%v", tc.payload, auto, err)
		}
	}
}

func TestPendingAdmissionRestoredFromStructuredEvents(t *testing.T) {
	for _, tc := range []struct {
		name        string
		typeOfEvent session.EventType
		payload     string
		want        bool
	}{
		{"failed start", session.EventToolProposed, `{"turn_id":"turn","tool":"goal_start"}`, true},
		{"premature report", session.EventToolProposed, `{"turn_id":"turn","tool":"goal_report"}`, true},
		{"blocked write", session.EventToolFailed, `{"turn_id":"turn","tool":"workspace_write","error_code":"goal_admission_required"}`, true},
		{"crash before guard", session.EventToolProposed, `{"turn_id":"turn","tool":"custom_write","risk_class":"write"}`, true},
		{"read", session.EventToolProposed, `{"turn_id":"turn","tool":"workspace_read","risk_class":"read"}`, false},
		{"other turn", session.EventToolProposed, `{"turn_id":"other","tool":"goal_start"}`, false},
		{"prose is not state", session.EventToolFailed, `{"turn_id":"turn","tool":"read","error":"goal_admission_required goal_start"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := memorystore.New()
			_, err := store.Append(context.Background(), "session", session.AnySequence, session.PendingEvent{Type: tc.typeOfEvent, Payload: json.RawMessage(tc.payload)})
			if err != nil {
				t.Fatal(err)
			}
			factory := &Factory{events: store}
			got, err := factory.AdmissionRequestedForTurn(context.Background(), "session", "turn")
			if err != nil || got != tc.want {
				t.Fatalf("requested=%t want=%t err=%v", got, tc.want, err)
			}
		})
	}
}

func TestRecordedTurnModeOverridesCurrentMode(t *testing.T) {
	for _, tc := range []struct {
		name          string
		payload       string
		current, want bool
	}{
		{"direct checkpoint in goal session", `{"goal_mode":false}`, true, false},
		{"goal checkpoint in direct session", `{"goal_mode":true}`, false, true},
		{"legacy goal checkpoint", `{}`, true, true},
		{"legacy direct checkpoint", `{}`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := memorystore.New()
			_, err := store.Append(context.Background(), "session", session.AnySequence, session.PendingEvent{
				Type: session.EventTurnStarted, CorrelationID: "turn", Payload: json.RawMessage(tc.payload),
			})
			if err != nil {
				t.Fatal(err)
			}
			factory := &Factory{events: store, goalMode: agentgoal.Config{Enabled: tc.current}}
			mode, err := factory.goalModeForTurn(context.Background(), "session", "turn")
			if err != nil || mode != tc.want {
				t.Fatalf("recorded mode=%v want=%v err=%v", mode, tc.want, err)
			}
		})
	}
}

func TestDirectTurnCountSurvivesCompactionAndExcludesFailures(t *testing.T) {
	store := memorystore.New()
	for _, tc := range []struct {
		id, payload string
		terminal    session.EventType
	}{
		{"legacy", `{}`, session.EventTurnCompleted},
		{"direct", `{"goal_mode":false}`, session.EventTurnCompleted},
		{"failed", `{"goal_mode":false}`, session.EventTurnFailed},
	} {
		_, err := store.Append(context.Background(), "session", session.AnySequence,
			session.PendingEvent{Type: session.EventTurnStarted, CorrelationID: tc.id, Payload: json.RawMessage(tc.payload)},
			session.PendingEvent{Type: tc.terminal, CorrelationID: tc.id, Payload: json.RawMessage(`{}`)},
		)
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err := store.Append(context.Background(), "session", session.AnySequence, session.PendingEvent{
		Type: session.EventContextCompacted, Payload: json.RawMessage(`{"summary":"compacted history"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	factory := &Factory{events: store}
	count, err := factory.CountCompletedUserTurns(context.Background(), "session")
	if err != nil || count != 2 {
		t.Fatalf("count=%d want=2 err=%v", count, err)
	}
}

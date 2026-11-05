package agentgoal

import (
	"context"
	"errors"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

type admissionLookup struct {
	Store
	goal  Goal
	found bool
	err   error
	reads int
}

func (store *admissionLookup) GoalForTurn(context.Context, session.ID, string) (Goal, Round, bool, error) {
	store.reads++
	return store.goal, Round{}, store.found, store.err
}

func TestAdmissionGuardUsesTaskPhaseNotToolUseAlone(t *testing.T) {
	ctx := context.Background()
	store := &admissionLookup{}
	guard := NewAdmissionGuard(store, "session", "turn", false)
	call := tool.Call{SessionID: "session", TurnID: "turn"}
	for _, risk := range []tool.RiskClass{tool.RiskPure, tool.RiskRead, tool.RiskExecute} {
		if err := guard.BeforeCall(ctx, call, tool.Descriptor{Name: "inspect", RiskClass: risk}); err != nil {
			t.Fatal(err)
		}
	}
	if err := guard.BeforeComplete(ctx, "session", "turn"); err != nil || guard.Requested() || store.reads != 0 {
		t.Fatalf("ordinary tool-assisted answer entered goal mode: %v", err)
	}
	if err := guard.BeforeCall(ctx, call, tool.Descriptor{Name: StartToolName, RiskClass: tool.RiskPure}); err != nil {
		t.Fatal(err)
	}
	for _, descriptor := range []tool.Descriptor{
		{Name: "write", RiskClass: tool.RiskWrite},
		{Name: "shell", RiskClass: tool.RiskExecute},
		{Name: "secret", RiskClass: tool.RiskSecret},
		{Name: ReportToolName, RiskClass: tool.RiskPure},
	} {
		var precondition *tool.PreconditionError
		if err := guard.BeforeCall(ctx, call, descriptor); !errors.As(err, &precondition) || precondition.Code != AdmissionRequiredCode {
			t.Fatalf("unadmitted call %s: %v", descriptor.Name, err)
		}
	}
	if err := guard.BeforeCall(ctx, call, tool.Descriptor{Name: "read", RiskClass: tool.RiskRead}); err != nil {
		t.Fatalf("blocked corrective inspection: %v", err)
	}
	if err := guard.BeforeComplete(ctx, "session", "turn"); err == nil {
		t.Fatal("unadmitted task marked complete")
	}
	store.found, store.goal.State = true, StateRunning
	if err := guard.BeforeCall(ctx, call, tool.Descriptor{Name: "write", RiskClass: tool.RiskWrite}); err != nil {
		t.Fatal(err)
	}
	if err := guard.BeforeComplete(ctx, "session", "turn"); err != nil {
		t.Fatal(err)
	}
	store.goal.State = StateCompleted
	if err := guard.BeforeCall(ctx, call, tool.Descriptor{Name: ReportToolName}); err == nil {
		t.Fatal("terminal goal accepted as active")
	}
	store.err = errors.New("storage unavailable")
	if err := guard.BeforeComplete(ctx, "session", "turn"); !errors.Is(err, store.err) {
		t.Fatalf("storage failure was hidden as a protocol error: %v", err)
	}
}

func TestAdmissionGuardRestoredPendingStateIsTurnScoped(t *testing.T) {
	store := &admissionLookup{}
	guard := NewAdmissionGuard(store, "session", "turn", true)
	ctx := tool.WithTurnGuard(context.Background(), guard)
	for _, call := range []tool.Call{{SessionID: "child", TurnID: "turn"}, {SessionID: "session", TurnID: "child"}} {
		if err := guard.BeforeCall(ctx, call, tool.Descriptor{Name: "write", RiskClass: tool.RiskWrite}); err != nil {
			t.Fatal(err)
		}
		if err := tool.CheckTurnCompletion(ctx, call.SessionID, call.TurnID); err != nil {
			t.Fatal(err)
		}
	}
	if store.reads != 0 {
		t.Fatal("parent guard queried child task state")
	}
	if err := tool.CheckTurnCompletion(ctx, "session", "turn"); err == nil {
		t.Fatal("restored pending task completed without registration")
	}
}

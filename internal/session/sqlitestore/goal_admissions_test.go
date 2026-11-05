package sqlitestore

import (
	"context"
	"errors"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

func admissionFixture(t *testing.T) (*OrchestrationStore, agentgoal.AdmissionRequest) {
	t.Helper()
	database, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	root, _, err := database.Orchestration().EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: "admission-session"})
	if err != nil {
		t.Fatal(err)
	}
	return database.Orchestration(), agentgoal.AdmissionRequest{Token: "admit-token", GoalID: "goal-1", SessionID: string(root.SessionID), TeamID: root.TeamID, AgentID: root.AgentID, InputDigest: "input", ContractDigest: "contract", ConfigDigest: "config"}
}

func TestGoalAdmissionCASHandshakeAndReplay(t *testing.T) {
	ctx := context.Background()
	store, request := admissionFixture(t)
	admission, err := store.Prepare(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if admission.State != agentgoal.AdmissionReserved || admission.Version != 1 {
		t.Fatalf("prepared admission=%+v", admission)
	}
	replayed, err := store.Prepare(ctx, request)
	if err != nil || replayed.Token != admission.Token || replayed.Version != admission.Version {
		t.Fatalf("prepare replay=%+v err=%v", replayed, err)
	}
	newToken := request
	newToken.Token = "lost-token-recovered"
	recovered, err := store.Prepare(ctx, newToken)
	if err != nil || recovered.Token != admission.Token {
		t.Fatalf("recover existing Goal token=%+v err=%v", recovered, err)
	}
	changed := request
	changed.InputDigest = "changed"
	if _, err := store.Prepare(ctx, changed); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("changed prepare=%v", err)
	}
	admission, err = store.BindTurn(ctx, admission.Token, "turn-1", "input", admission.Version)
	if err != nil {
		t.Fatal(err)
	}
	if admission.State != agentgoal.AdmissionTurnStarted || admission.TurnID != "turn-1" || admission.Version != 2 {
		t.Fatalf("bound turn=%+v", admission)
	}
	if _, err := store.BindTurn(ctx, admission.Token, "turn-other", "input", 2); !errors.Is(err, agentgoal.ErrAdmissionTransition) {
		t.Fatalf("identity mismatch=%v", err)
	}
	admission, err = store.BindTurn(ctx, admission.Token, "turn-1", "input", 2)
	if err != nil || admission.Version != 2 {
		t.Fatalf("turn replay=%+v err=%v", admission, err)
	}
	admission, err = store.BindRound(ctx, admission.Token, "goal-1", 1, admission.Version)
	if err != nil {
		t.Fatal(err)
	}
	if admission.State != agentgoal.AdmissionRoundStarted || admission.Round != 1 || admission.Version != 3 {
		t.Fatalf("bound round=%+v", admission)
	}
	admission, err = store.Consume(ctx, admission.Token, admission.Version)
	if err != nil || admission.State != agentgoal.AdmissionConsumed || admission.Version != 4 {
		t.Fatalf("consume=%+v err=%v", admission, err)
	}
	if _, err := store.MarkUnavailable(ctx, admission.Token, admission.Version, "late"); !errors.Is(err, agentgoal.ErrAdmissionTransition) {
		t.Fatalf("consumed unavailable=%v", err)
	}
}

func TestGoalAdmissionUnavailableIsDurableAndReopenable(t *testing.T) {
	ctx := context.Background()
	store, request := admissionFixture(t)
	admission, err := store.Prepare(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	admission, err = store.MarkUnavailable(ctx, admission.Token, admission.Version, "missing round binding")
	if err != nil || admission.State != agentgoal.AdmissionUnavailable {
		t.Fatalf("unavailable=%+v err=%v", admission, err)
	}
	admission, err = store.MarkUnavailable(ctx, admission.Token, admission.Version, "missing round binding")
	if err != nil || admission.State != agentgoal.AdmissionUnavailable {
		t.Fatalf("unavailable replay=%+v err=%v", admission, err)
	}
	values, err := store.List(ctx, agentgoal.AdmissionUnavailable)
	if err != nil || len(values) != 1 || values[0].Reason != "missing round binding" {
		t.Fatalf("list=%+v err=%v", values, err)
	}
	if _, err := store.BindTurn(ctx, request.Token, "turn-1", request.InputDigest, admission.Version); !errors.Is(err, agentgoal.ErrAdmissionTransition) {
		t.Fatalf("unavailable bind=%v", err)
	}
}

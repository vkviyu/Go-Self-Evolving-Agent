package atestore

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentflow"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

func flowTestRequest(t *testing.T, store *Store) agentflow.RunRequest {
	t.Helper()
	team, agent := usageTestOwner(t, store)
	input, err := agentflow.NewValue("task")
	if err != nil {
		t.Fatal(err)
	}
	return agentflow.RunRequest{ID: "flow", IdempotencyKey: "resume", Definition: "identity",
		DefinitionRevision: "v1", DefinitionFingerprint: strings.Repeat("a", 64), SessionID: "flow-session",
		TeamID: team, AgentID: agent, Input: input, RegistryGeneration: 1, RuntimeSnapshotID: "snapshot"}
}

func flowTestEvents(t *testing.T, store *Store, runID string) []orchestration.Event {
	t.Helper()
	var result []orchestration.Event
	if err := store.authority.View(context.Background(), func(tx *ateauthority.Tx) error {
		events, err := listRecords[orchestration.Event](tx, "event")
		if err != nil {
			return err
		}
		for _, event := range events {
			if event.StreamType == orchestration.StreamFlow && event.StreamID == runID {
				result = append(result, event)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestFlowsIdentityAndStepRetryEvents(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	request := flowTestRequest(t, store)
	first, err := store.CreateRun(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	request.ID, request.RegistryGeneration = "new-id", 99
	retry, err := store.CreateRun(ctx, request)
	if err != nil || retry.ID != first.ID || retry.RegistryGeneration != 1 {
		t.Fatalf("snapshot identity retry: %+v %v", retry, err)
	}
	request.RuntimeSnapshotID = "changed"
	if _, err := store.CreateRun(ctx, request); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("snapshot identity conflict: %v", err)
	}
	step, err := store.StartStep(ctx, first.ID, "step", "input")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartStep(ctx, first.ID, "step", "changed"); !errors.Is(err, agentflow.ErrStepConflict) {
		t.Fatalf("step input conflict: %v", err)
	}
	if err := store.FailStep(ctx, first.ID, "step", "retryable"); err != nil {
		t.Fatal(err)
	}
	if err := store.FailStep(ctx, first.ID, "step", "retryable"); err != nil {
		t.Fatal(err)
	}
	restarted, err := store.StartStep(ctx, first.ID, "step", "input")
	if err != nil || restarted.Attempt != 2 || restarted.Error != "" || restarted.CompletedAt != nil || !restarted.StartedAt.Equal(step.StartedAt) {
		t.Fatalf("step restart: %+v %v", restarted, err)
	}
	if events := flowTestEvents(t, store, first.ID); len(events) != 3 {
		t.Fatalf("duplicate retry events: %+v", events)
	}
	output, err := agentflow.NewValue("done")
	if err != nil {
		t.Fatal(err)
	}
	outputs := []agentflow.Value{output}
	completed, err := store.FinishStep(ctx, first.ID, "step", outputs)
	if err != nil || completed.State != agentflow.StepCompleted || completed.CompletedAt == nil {
		t.Fatalf("step finish: %+v %v", completed, err)
	}
	if _, err := store.FinishStep(ctx, first.ID, "step", outputs); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinishStep(ctx, first.ID, "step", nil); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("completed outputs changed: %v", err)
	}
	run, err := store.FinishRun(ctx, first.ID, agentflow.StateCompleted, outputs, "")
	if err != nil || run.Version != 2 || run.CompletedAt == nil {
		t.Fatalf("run finish: %+v %v", run, err)
	}
	if _, err := store.FinishRun(ctx, first.ID, agentflow.StateCompleted, outputs, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinishRun(ctx, first.ID, agentflow.StateFailed, outputs, "error"); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("terminal run changed: %v", err)
	}
	if _, err := store.StartStep(ctx, first.ID, "new", "input"); err == nil {
		t.Fatal("started step on stopped run")
	}
	if events := flowTestEvents(t, store, first.ID); len(events) != 5 {
		t.Fatalf("event count: %+v", events)
	}
}

func TestFlowsSessionStagesKeepOriginalActionAndTeamOwnership(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	request := flowTestRequest(t, store)
	if _, err := store.CreateRun(ctx, request); err != nil {
		t.Fatal(err)
	}
	stage, err := store.BeginSessionStage(ctx, request.ID, "worker", "first")
	if err != nil || stage.Action != agentflow.SessionSpawn {
		t.Fatalf("initial stage: %+v %v", stage, err)
	}
	if err := store.BindSessionAgent(ctx, request.ID, "worker", request.AgentID); err != nil {
		t.Fatal(err)
	}
	stage, err = store.BeginSessionStage(ctx, request.ID, "worker", "first")
	if err != nil || stage.Action != agentflow.SessionSpawn {
		t.Fatalf("replay changed action: %+v %v", stage, err)
	}
	stage, err = store.BeginSessionStage(ctx, request.ID, "worker", "second")
	if err != nil || stage.Action != agentflow.SessionContinue {
		t.Fatalf("continuation: %+v %v", stage, err)
	}
	if id, found, err := store.SessionAgent(ctx, request.ID, "worker"); err != nil || !found || id != request.AgentID {
		t.Fatalf("session agent: %s %t %v", id, found, err)
	}
	_, foreign := usageTestOwner(t, store)
	if err := store.BindSessionAgent(ctx, request.ID, "foreign", foreign); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("cross-team binding accepted: %v", err)
	}
	if _, found, err := store.SessionAgent(ctx, request.ID, "foreign"); err != nil || found {
		t.Fatalf("failed binding persisted: %t %v", found, err)
	}
	other, err := orchestration.NewAgentID()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		return createRecord(tx, "agent", string(other), orchestration.AgentSnapshot{ID: other, TeamID: request.TeamID})
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.BindSessionAgent(ctx, request.ID, "worker", other); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("binding replaced: %v", err)
	}
}

func TestFlowsFailureRollsBackRecordsAndEvents(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	request := flowTestRequest(t, store)
	// Force event sequencing to fail after run and request index are staged.
	if err := store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		return putRecord(tx, "event-global-counter", "global", "corrupt-counter")
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateRun(ctx, request); err == nil {
		t.Fatal("corrupt event counter accepted")
	}
	if _, err := store.GetRun(ctx, request.ID); !errors.Is(err, orchestration.ErrNotFound) {
		t.Fatalf("run survived aborted event append: %v", err)
	}
	if err := store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		return putRecord(tx, "event-global-counter", "global", uint64(0))
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateRun(ctx, request); err != nil {
		t.Fatalf("request index survived aborted creation: %v", err)
	}
	if _, err := store.StartStep(ctx, request.ID, "step", "input"); err != nil {
		t.Fatal(err)
	}
	bad := []agentflow.Value{{Data: json.RawMessage("{")}}
	if _, err := store.FinishStep(ctx, request.ID, "step", bad); err == nil {
		t.Fatal("invalid output accepted")
	}
	if step, err := store.StartStep(ctx, request.ID, "step", "input"); err != nil || step.State != agentflow.StepRunning {
		t.Fatalf("invalid output changed state: %+v %v", step, err)
	}
}

func TestFlowsReferencesGenerationIdentityAndNullOutputs(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	request := flowTestRequest(t, store)
	missing, err := orchestration.NewAgentID()
	if err != nil {
		t.Fatal(err)
	}
	invalid := request
	invalid.AgentID = missing
	if _, err := store.CreateRun(ctx, invalid); !errors.Is(err, orchestration.ErrNotFound) {
		t.Fatalf("missing Agent accepted: %v", err)
	}
	request.RuntimeSnapshotID = ""
	run, err := store.CreateRun(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	request.RegistryGeneration++
	if _, err := store.CreateRun(ctx, request); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("generation changed without content identity: %v", err)
	}
	if _, err := store.StartStep(ctx, run.ID, "empty", "input"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinishStep(ctx, run.ID, "empty", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinishStep(ctx, run.ID, "empty", []agentflow.Value{}); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("null and empty outputs conflated: %v", err)
	}
}

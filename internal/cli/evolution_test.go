package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/vkviyu/go-self-evolving-agent/internal/modelgateway"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/retention"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/strategy"
)

func TestMaintenanceAndStrategyValidationStayOffline(t *testing.T) {
	cliATEEnvironment(t)
	ctx := context.Background()
	data, workspace := t.TempDir(), t.TempDir()
	db := openCLIApp(t, cliCommandConfig(t, data, workspace))
	if _, _, err := db.Orchestration().EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "finished"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Events().Append(ctx, "finished", session.AnySequence, session.PendingEvent{Type: session.EventTurnStarted, CorrelationID: "turn", Payload: []byte(`{}`)}, session.PendingEvent{Type: session.EventTurnCompleted, CorrelationID: "turn", Payload: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	deps := dependencies{newModel: func(context.Context, modelgateway.Config) (model.ToolCallingChatModel, error) {
		t.Fatal("offline command constructed provider")
		return nil, nil
	}}
	runCommand := func(args []string, want int) []byte {
		t.Helper()
		var out, log bytes.Buffer
		args = append(args, "--data-dir", data)
		code := runWithDependencies(ctx, args, unreadAbandonmentInput{t}, &out, &log, deps)
		if code != want {
			t.Fatalf("%v code=%d log=%s", args, code, log.String())
		}
		return out.Bytes()
	}
	raw := runCommand([]string{"maintenance", "plan", "--session", "finished"}, 0)
	var plan retention.Plan
	if err := json.Unmarshal(raw, &plan); err != nil || len(plan.Consumers) != 1 {
		t.Fatalf("plan=%s %v", raw, err)
	}
	runCommand([]string{"maintenance", "archive", "--session", "finished", "--plan-id", plan.ID}, 1)
	runCommand([]string{"maintenance", "archive", "--session", "finished", "--plan-id", plan.ID, "--confirm"}, 0)
	runCommand([]string{"maintenance", "compact", "--confirm"}, 0)
	for _, args := range [][]string{
		{"maintenance", "plan", "--session", "finished", "--confirm"},
		{"strategy", "publish", "--report-id", "fake"},
		{"strategy", "shadow", "--candidate-id", "fake", "--suite-id", "fake", "--confirm", "--workspace", workspace},
	} {
		runCommand(args, 1)
	}
}

func TestStrategyCLIUsesSharedChatConfigAndExplicitModelConsent(t *testing.T) {
	cliATEEnvironment(t)
	ctx := context.Background()
	data, workspace := t.TempDir(), t.TempDir()
	writeJSON := func(name string, value any) string {
		t.Helper()
		raw, _ := json.Marshal(value)
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	specPath := writeJSON("spec.json", strategy.Spec{Version: 1, Revision: "cli-v1", InstructionSuffix: "keep exact replies"})
	suitePath := writeJSON("suite.json", strategy.Suite{Name: "trusted", Fixtures: []strategy.Fixture{{Name: "safety", Prompt: "OK", Expected: "reply: OK", Safety: true}}})
	calls := 0
	deps := dependencies{newModel: func(context.Context, modelgateway.Config) (model.ToolCallingChatModel, error) {
		calls++
		return &chatTestModel{}, nil
	}}
	runCommand := func(args []string) map[string]json.RawMessage {
		t.Helper()
		var out, log bytes.Buffer
		code := runWithDependencies(ctx, args, unreadAbandonmentInput{t}, &out, &log, deps)
		if code != 0 {
			t.Fatalf("%v code=%d log=%s output=%s", args, code, log.String(), out.String())
		}
		var result map[string]json.RawMessage
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	common := []string{"--data-dir", data, "--workspace", workspace}
	proposed := runCommand(append([]string{"strategy", "propose", "--file", specPath}, common...))
	registered := runCommand(append([]string{"strategy", "suite", "--file", suitePath, "--confirm"}, common...))
	var candidateID, suiteID string
	json.Unmarshal(proposed["candidate_id"], &candidateID)
	json.Unmarshal(registered["suite_id"], &suiteID)
	if calls != 0 {
		t.Fatal("offline registration constructed model")
	}
	args := append([]string{"strategy", "shadow", "--candidate-id", candidateID, "--suite-id", suiteID, "--confirm"}, common...)
	args = append(args, "--", "--model", "test", "--session", "shadow-owner")
	compared := runCommand(args)
	var reportID string
	json.Unmarshal(compared["report_id"], &reportID)
	if calls != 1 || reportID == "" {
		t.Fatalf("model constructions=%d report=%s", calls, reportID)
	}
	runCommand(append([]string{"strategy", "publish", "--report-id", reportID, "--confirm"}, common...))
	if calls != 1 {
		t.Fatal("publish contacted model")
	}
}

package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentmanager"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentprofile"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/approval"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	pluginwasm "github.com/vkviyu/go-self-evolving-agent/internal/plugin/wasm"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func TestZeroAppCloseIsSafeAndIdempotent(t *testing.T) {
	var application App
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestClosedAppRejectsSupervisorInitialization(t *testing.T) {
	application, err := Open(context.Background(), config.Config{
		DataDirectory: t.TempDir(),
		WASM:          pluginwasm.Config{DefaultTimeout: time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := application.NewSupervisor(agentmanager.Config{}); err == nil {
		t.Fatal("closed application accepted Supervisor initialization")
	}
	if _, err := application.NewAgentFactory(nil, agentruntime.Config{}); err == nil {
		t.Fatal("closed application accepted Agent factory initialization")
	}
}

func TestDoctorRunsThroughDurablePipeline(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	application, err := Open(context.Background(), config.Config{
		DataDirectory: directory,
		WASM:          pluginwasm.Config{DefaultTimeout: time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := application.Close(); err != nil {
			t.Error(err)
		}
	})
	report, err := application.Doctor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || report.RegistryGeneration != 2 {
		t.Fatalf("unexpected report: %#v", report)
	}
	if report.DatabasePath != filepath.Join(directory, "sessions.db") {
		t.Fatalf("database path = %q", report.DatabasePath)
	}
	// This configuration names no rendezvous directory, which the report
	// must surface instead of claiming cross-home exclusion.
	if coordination := report.Coordination; coordination.WorkspaceID == "" || coordination.InstanceID == "" || coordination.CrossHomeExclusion || coordination.Degraded == "" {
		t.Fatalf("coordination report = %#v", coordination)
	}
	if application.WorkspaceCoordinator() == nil {
		t.Fatal("workspace coordinator is not installed")
	}
	if application.FlowRuntime() == nil || application.FlowCatalog() == nil {
		t.Fatal("Flow runtime and catalog are not installed")
	}
	snapshot := application.Registry().Snapshot()
	if _, ok := snapshot.Get("flow_list"); !ok {
		t.Fatal("flow_list is not registered")
	}
	if _, ok := snapshot.Get("flow_run"); !ok {
		t.Fatal("flow_run is not registered")
	}
	if _, ok := snapshot.Get("artifact_read"); !ok {
		t.Fatal("artifact_read is not registered")
	}
	want := []session.EventType{
		session.EventSessionCreated,
		session.EventToolProposed,
		session.EventPolicyDecided,
		session.EventToolStarted,
		session.EventToolSucceeded,
	}
	if len(report.EventTypes) != len(want) {
		t.Fatalf("event types = %v", report.EventTypes)
	}
	for index := range want {
		if report.EventTypes[index] != want[index] {
			t.Fatalf("event %d = %q, want %q", index, report.EventTypes[index], want[index])
		}
	}
}

func TestNewSupervisorCleansBindingsWhenRecoveryFails(t *testing.T) {
	t.Parallel()
	application, err := Open(context.Background(), config.Config{
		DataDirectory: t.TempDir(),
		WASM:          pluginwasm.Config{DefaultTimeout: time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	failingAgents := failingAgentRepository{AgentRepository: application.Orchestration()}
	if _, err := application.NewSupervisor(agentmanager.Config{Agents: failingAgents}); err == nil {
		t.Fatal("expected Supervisor recovery failure")
	}
	if _, ok := application.Registry().Snapshot().Get("agent_spawn"); ok {
		t.Fatal("failed Supervisor left agent_spawn binding active")
	}
	if _, err := application.NewSupervisor(agentmanager.Config{}); err != nil {
		t.Fatalf("second Supervisor initialization failed after cleanup: %v", err)
	}
}

type failingAgentRepository struct {
	orchestration.AgentRepository
}

func TestNewSupervisorRejectsExistingControlToolOwner(t *testing.T) {
	application, err := Open(context.Background(), config.Config{DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	binding := tool.Binding{
		Descriptor: tool.Descriptor{Name: "agent_spawn", Revision: "other-owner", Description: "Reserved control name", InputSchema: []byte(`{"type":"object"}`), RiskClass: tool.RiskPure},
		Executor:   tool.InvokeFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) { return input, nil }),
	}
	before, err := application.Registry().Activate(binding)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := application.NewSupervisor(agentmanager.Config{}); err == nil {
		t.Fatal("Supervisor replaced another owner's tool")
	}
	if application.Registry().Snapshot() != before {
		t.Fatal("failed Supervisor changed registry")
	}
	if _, err := application.Registry().Retire("agent_spawn", "other-owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := application.NewSupervisor(agentmanager.Config{}); err != nil {
		t.Fatalf("Supervisor resources not released: %v", err)
	}
}

func (repository failingAgentRepository) ListTeams(context.Context) ([]orchestration.TeamID, error) {
	return nil, errors.New("synthetic recovery failure")
}

func TestApprovalSourceNamesDelegatedAgentProfileAndTask(t *testing.T) {
	application, err := Open(context.Background(), config.Config{DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	ctx := context.Background()
	store := application.Orchestration()
	root, _, err := application.EnsureRootThread(ctx, "approval-source")
	if err != nil {
		t.Fatal(err)
	}
	worker, ok := application.Profiles().Snapshot().Get("worker")
	if !ok {
		t.Fatal("worker profile is not registered")
	}
	snapshot, err := agentprofile.Encode(worker)
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.CreateChild(ctx, orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: worker.Revision, ProfileSnapshot: snapshot})
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, orchestration.TaskSpec{TeamID: root.TeamID, ActorAgentID: root.AgentID, Title: "Collect release notes"})
	if err != nil {
		t.Fatal(err)
	}
	describe := approvalSource(store)
	if source := describe(ctx, root.ActorScope()); source != (approval.Source{}) {
		t.Fatalf("root request was labeled: %#v", source)
	}
	scope := child.ActorScope()
	scope.TaskID = task.ID
	if source := describe(ctx, scope); source.AgentID != child.ID || source.Profile != "worker" || source.Task != "Collect release notes" {
		t.Fatalf("child source = %#v", source)
	}
}

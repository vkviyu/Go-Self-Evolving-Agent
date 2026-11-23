package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
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

var appTestATE struct {
	once      sync.Once
	directory string
	binary    string
	err       error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if appTestATE.directory != "" {
		if err := os.RemoveAll(appTestATE.directory); err != nil {
			fmt.Fprintln(os.Stderr, "remove App test ATE binary:", err)
			code = 1
		}
	}
	os.Exit(code)
}

// The binary belongs to the package, not the first test that happens to use it.
// Individual tests retain private native stores and can run in parallel.
func testATEBinary(t *testing.T) string {
	t.Helper()
	appTestATE.once.Do(func() {
		appTestATE.directory, appTestATE.err = os.MkdirTemp("", "gsea-app-test-ate-")
		if appTestATE.err != nil {
			return
		}
		appTestATE.binary = filepath.Join(appTestATE.directory, "ate")
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		command := exec.CommandContext(ctx, "go", "build", "-o", appTestATE.binary, "github.com/vkviyu/agent-temporal-engine/cmd/ate")
		command.Dir = temporalRepo(t)
		if output, err := command.CombinedOutput(); err != nil {
			appTestATE.err = fmt.Errorf("build native ATE: %w\n%s", err, output)
		}
	})
	if appTestATE.err != nil {
		t.Fatal(appTestATE.err)
	}
	return appTestATE.binary
}

func testConfig(t *testing.T, cfg config.Config) config.Config {
	t.Helper()
	if cfg.DataDirectory == "" {
		cfg.DataDirectory = t.TempDir()
	}
	if cfg.HomeDirectory == "" {
		cfg.HomeDirectory = filepath.Join(cfg.DataDirectory, "home")
	}
	if cfg.WorkspaceDirectory == "" {
		cfg.WorkspaceDirectory = filepath.Join(cfg.DataDirectory, "workspace")
		if err := os.MkdirAll(cfg.WorkspaceDirectory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cfg.ATE.Enabled = true
	if cfg.ATE.Binary == "" {
		cfg.ATE.Binary = testATEBinary(t)
	}
	if cfg.ATE.Directory == "" {
		cfg.ATE.Directory = filepath.Join(cfg.DataDirectory, "ate")
	}
	if cfg.ATE.Timeout == 0 {
		cfg.ATE.Timeout = 30 * time.Second
	}
	return cfg
}

func testOpen(t *testing.T, ctx context.Context, cfg config.Config) (*App, error) {
	t.Helper()
	a, err := Open(ctx, testConfig(t, cfg))
	if err == nil {
		// Keep production native authority and process ownership. These tests
		// script ordinary model turns, not semantic experience maintenance.
		a.experience = nil
	}
	return a, err
}

func testOpenWithOptions(t *testing.T, ctx context.Context, cfg config.Config, options OpenOptions) (*App, error) {
	t.Helper()
	a, err := OpenWithOptions(ctx, testConfig(t, cfg), options)
	if err == nil {
		a.experience = nil
	}
	return a, err
}

func TestZeroAppCloseIsSafeAndIdempotent(t *testing.T) {
	var application App
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
}

// Exercise the public assembly paths without access to the SQLite owner. The
// interface wrapper also prevents incidental concrete-store methods from being
// available to consumers through dynamic type assertions.
func TestAppRepositoriesAreIndependentOfDatabaseOwner(t *testing.T) {
	ctx := t.Context()
	application, err := testOpen(t, ctx, config.Config{
		DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	database := application.database
	t.Cleanup(func() {
		application.database = database
		if err := application.Close(); err != nil {
			t.Error(err)
		}
	})
	repositories := struct{ OrchestrationRepository }{application.repositories}
	application.repositories = repositories
	application.database = nil
	if application.Orchestration() != repositories {
		t.Fatal("Orchestration did not return the configured repositories")
	}
	root, _, err := application.EnsureRootThread(ctx, "repository-boundary")
	if err != nil {
		t.Fatal(err)
	}
	_, err = application.NewAgentFactory(usageModel{}, agentruntime.Config{ActorScope: root.ActorScope()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := application.Recoveries(ctx, root.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := application.RuntimeSnapshotReferences(ctx, "missing"); !errors.Is(err, tool.ErrSnapshotUnavailable) {
		t.Fatalf("snapshot reference lookup did not reach runtime storage: %v", err)
	}
	if _, err := application.RuntimeSnapshotReferenceHistory(ctx, "missing"); !errors.Is(err, tool.ErrSnapshotUnavailable) {
		t.Fatalf("snapshot reference history did not reach runtime storage: %v", err)
	}
	executor, err := application.NewMergeExecutor(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if executor.Repository != repositories || executor.Journal != repositories {
		t.Fatal("merge executor lost the configured repositories or apply journal")
	}
	if _, err := application.NewSupervisor(agentmanager.Config{
		Runner: agentmanager.RunnerFunc(func(context.Context, agentmanager.RunRequest) (orchestration.AgentResult, error) {
			return orchestration.AgentResult{}, errors.New("unexpected child execution")
		}),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestClosedAppRejectsSupervisorInitialization(t *testing.T) {
	application, err := testOpen(t, context.Background(), config.Config{
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
	application, err := testOpen(t, context.Background(), config.Config{
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
	if report.DatabasePath != filepath.Join(directory, "runtime-v3.db") {
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
	application, err := testOpen(t, context.Background(), config.Config{
		DataDirectory: t.TempDir(),
		WASM:          pluginwasm.Config{DefaultTimeout: time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	if _, _, err := application.EnsureRootThread(t.Context(), "supervisor-recovery"); err != nil {
		t.Fatal(err)
	}
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

func TestNewSupervisorContextSettlesPendingCompletionInStorageBatch(t *testing.T) {
	application, err := testOpen(t, t.Context(), config.Config{
		DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	var supervisor *agentmanager.Supervisor
	var child orchestration.AgentSnapshot
	ctx, starts := storageBatchObserved(t.Context())
	err = application.WithStorageBatch(ctx, func(batchContext context.Context) error {
		root, _, err := application.EnsureRootThread(batchContext, "supervisor-batch-completion")
		if err != nil {
			return err
		}
		store := application.Orchestration()
		child, err = store.CreateChild(batchContext, orchestration.SpawnSpec{
			TeamID: root.TeamID, ParentAgentID: root.AgentID,
			ProfileRevision: "worker", InitialPrompt: "already executed",
			Workspace: orchestration.WorkspaceReadonly,
		})
		if err != nil {
			return err
		}
		runID, err := orchestration.NewRunID()
		if err != nil {
			return err
		}
		child, err = store.TransitionAgent(batchContext, orchestration.AgentTransition{
			AgentID: child.ID, Expected: child.Version, Target: orchestration.AgentRunning, RunID: runID,
		})
		if err != nil {
			return err
		}
		if _, err = store.PrepareAgentCompletion(batchContext, orchestration.AgentCompletion{
			AgentID: child.ID, RunID: runID, Result: orchestration.AgentResult{Status: orchestration.ResultSucceeded, Summary: "frozen result"},
		}); err != nil {
			return err
		}
		supervisor, err = application.NewSupervisorContext(batchContext, agentmanager.Config{
			RunContext: t.Context(), DeferRecoveryDispatch: true, WaitPollInterval: time.Millisecond,
			Runner: agentmanager.RunnerFunc(func(context.Context, agentmanager.RunRequest) (orchestration.AgentResult, error) {
				t.Error("recovery reran a completed worker")
				return orchestration.AgentResult{}, errors.New("unexpected worker")
			}),
		})
		if err != nil {
			return err
		}
		recovered, err := store.GetAgent(batchContext, child.ID)
		if err != nil {
			return err
		}
		if recovered.State != orchestration.AgentCompleted || recovered.Result == nil || recovered.Result.Summary != "frozen result" {
			return fmt.Errorf("pending completion did not settle: %+v", recovered)
		}
		pending, err := store.PendingAgentCompletions(batchContext, root.TeamID)
		if err != nil {
			return err
		}
		if len(pending) != 0 {
			return fmt.Errorf("pending receipts remain: %+v", pending)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 0 {
		t.Fatalf("recovery opened %d additional storage processes; want persistent lease reuse", starts.Load())
	}
	// The original RunContext remains usable after the batch lease expires.
	supervisor.StartAvailableAsync()
	if err := supervisor.Close(); err != nil {
		t.Fatal(err)
	}
}

type failingAgentRepository struct {
	orchestration.AgentRepository
}

type failingPendingCompletionRepository struct {
	OrchestrationRepository
}

func (repository failingPendingCompletionRepository) PendingAgentCompletions(context.Context, orchestration.TeamID) ([]orchestration.AgentCompletion, error) {
	return nil, errors.New("synthetic pending completion recovery failure")
}

func TestNewSupervisorContextFailureCleansUpInStorageBatch(t *testing.T) {
	application, err := testOpen(t, t.Context(), config.Config{
		DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	ctx, starts := storageBatchObserved(t.Context())
	err = application.WithStorageBatch(ctx, func(batchContext context.Context) error {
		root, _, err := application.EnsureRootThread(batchContext, "supervisor-batch-failure")
		if err != nil {
			return err
		}
		store := application.Orchestration()
		child, err := store.CreateChild(batchContext, orchestration.SpawnSpec{
			TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker",
			InitialPrompt: "cancel on failed construction", Workspace: orchestration.WorkspaceReadonly,
		})
		if err != nil {
			return err
		}
		_, err = application.NewSupervisorContext(batchContext, agentmanager.Config{
			Agents: failingPendingCompletionRepository{store}, RunContext: t.Context(), DeferRecoveryDispatch: true,
		})
		if err == nil {
			return errors.New("expected recovery failure")
		}
		if _, ok := application.Registry().Snapshot().Get("agent_spawn"); ok {
			return errors.New("failed supervisor retained control tools")
		}
		child, err = store.GetAgent(batchContext, child.ID)
		if err != nil {
			return err
		}
		if child.State != orchestration.AgentCanceled {
			return fmt.Errorf("failed supervisor did not clean up its child: %+v", child)
		}
		_, err = application.NewSupervisorContext(batchContext, agentmanager.Config{
			RunContext: t.Context(), DeferRecoveryDispatch: true,
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 0 {
		t.Fatalf("failed recovery cleanup opened %d additional storage processes; want persistent lease reuse", starts.Load())
	}
}

func TestNewSupervisorRejectsExistingControlToolOwner(t *testing.T) {
	application, err := testOpen(t, context.Background(), config.Config{DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	if _, _, err := application.EnsureRootThread(t.Context(), "supervisor-owner"); err != nil {
		t.Fatal(err)
	}
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
	application, err := testOpen(t, context.Background(), config.Config{DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir()})
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

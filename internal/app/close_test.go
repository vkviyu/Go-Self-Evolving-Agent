package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentmanager"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimeowner"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

func TestConcurrentCloseJoinsEvolutionEvidenceBeforeReleasingOwnership(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg := config.Config{DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir()}
	a, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	root, _, err := a.EnsureRootThread(ctx, "closing-evidence")
	if err != nil {
		t.Fatal(err)
	}
	worker, finish, err := a.beginEvolution(ctx)
	if err != nil {
		t.Fatal(err)
	}
	evidence := make(chan error, 1)
	go func() {
		defer finish()
		<-worker.Done()
		// Persistence deliberately detaches from the canceled worker. Close
		// must keep both the database and exclusive runtime ownership alive.
		persist, cancel := context.WithTimeout(context.WithoutCancel(worker), 5*time.Second)
		defer cancel()
		ownership := requireOwned(a.config.SessionDatabasePath())
		write := a.Orchestration().RecordUsage(persist, tokenusage.Record{
			TeamID: root.TeamID, AgentID: root.AgentID, RequestID: "shutdown-evidence",
			Usage: tokenusage.Snapshot{Input: 3, Output: 2, Total: 5},
		})
		evidence <- errors.Join(ownership, write)
	}()
	const callers = 8
	closed := make(chan error, callers)
	for range callers {
		go func() { closed <- a.Close() }()
	}
	for range callers {
		if err := <-closed; err != nil {
			t.Fatal(err)
		}
	}
	if err := <-evidence; err != nil {
		t.Fatalf("resources released before worker evidence: %v", err)
	}
	if _, err := a.Orchestration().ListTeams(ctx); err == nil {
		t.Fatal("successful Close left the database open")
	}
	report, err := ReadSessionUsage(ctx, cfg, root.SessionID)
	if err != nil || report.Aggregate.TotalTokens != 5 || report.Aggregate.Records != 1 {
		t.Fatalf("shutdown evidence not durable or owner not released: %+v, %v", report, err)
	}
}

func TestEvolutionCloseTimeoutRetainsResources(t *testing.T) {
	t.Parallel()
	a, err := Open(context.Background(), config.Config{DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	worker, finish, err := a.beginEvolution(context.Background())
	if err != nil {
		_ = a.Close()
		t.Fatal(err)
	}
	defer func() {
		// The fixture, unlike a wedged provider, can prove it has stopped.
		finish()
		closeRetainedTestResources(t, a)
	}()
	closeErr := a.Close()
	if closeErr == nil || !errors.Is(worker.Err(), context.Canceled) {
		t.Fatalf("timeout=%v worker=%v", closeErr, worker.Err())
	}
	assertTimedOutAppRetainsResources(t, a, closeErr)
}

// A blocked wait-monitor query is a real Supervisor worker. The explicit arm
// avoids blocking synchronous recovery during NewSupervisor. After release,
// it performs one final durable query before signaling, then only returns
// cancellation; test cleanup cannot race any later database access.
type blockedCloseRepository struct {
	orchestration.AgentRepository
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
	read    chan error
	done    chan struct{}
	once    sync.Once
}

func (r *blockedCloseRepository) ListTeams(ctx context.Context) ([]orchestration.TeamID, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !r.armed.CompareAndSwap(true, false) {
		return r.AgentRepository.ListTeams(ctx)
	}
	close(r.entered)
	defer close(r.done)
	<-r.release
	_, err := r.AgentRepository.ListTeams(context.WithoutCancel(ctx))
	r.read <- err
	return nil, context.Canceled
}

func TestSupervisorCloseTimeoutRetainsResources(t *testing.T) {
	t.Parallel()
	a, err := Open(context.Background(), config.Config{DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	repository := &blockedCloseRepository{
		AgentRepository: a.Orchestration(), entered: make(chan struct{}), release: make(chan struct{}), read: make(chan error, 1), done: make(chan struct{}),
	}
	if _, err := a.NewSupervisor(agentmanager.Config{Agents: repository, WaitPollInterval: time.Millisecond}); err != nil {
		_ = a.Close()
		t.Fatal(err)
	}
	defer func() {
		repository.once.Do(func() { close(repository.release) })
		_ = a.Close()
		select {
		case <-repository.entered:
			select {
			case <-repository.done:
			case <-time.After(5 * time.Second):
				t.Error("test worker still running; retaining its resources")
				return
			}
		default:
		}
		closeRetainedTestResources(t, a)
	}()
	repository.armed.Store(true)
	select {
	case <-repository.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Supervisor monitor did not enter its query")
	}
	closeErr := a.Close()
	if !errors.Is(closeErr, agentmanager.ErrSupervisorCloseTimeout) {
		t.Fatalf("close error = %v", closeErr)
	}
	// Use non-fatal assertions until the blocked worker has been released.
	assertTimedOutAppRetainsResources(t, a, closeErr)
	repository.once.Do(func() { close(repository.release) })
	select {
	case err := <-repository.read:
		if err != nil {
			t.Errorf("timed-out worker lost its database: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("released worker did not finish its durable query")
	}
}

type failedRecoveryCloseRepository struct {
	*blockedCloseRepository
	failure error
}

func (r *failedRecoveryCloseRepository) ListTeams(ctx context.Context) ([]orchestration.TeamID, error) {
	// App recovery uses a synchronous background context; the monitor owns
	// a cancellable context. Fail recovery only once its worker is live.
	if ctx.Done() == nil {
		select {
		case <-r.entered:
			return nil, r.failure
		case <-time.After(5 * time.Second):
			return nil, errors.New("monitor did not enter before recovery failure")
		}
	}
	return r.blockedCloseRepository.ListTeams(ctx)
}

func TestFailedSupervisorAssemblyRetainsTimedOutWorker(t *testing.T) {
	t.Parallel()
	a, err := Open(context.Background(), config.Config{DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	repository := &failedRecoveryCloseRepository{
		blockedCloseRepository: &blockedCloseRepository{
			AgentRepository: a.Orchestration(), entered: make(chan struct{}), release: make(chan struct{}), read: make(chan error, 1), done: make(chan struct{}),
		},
		failure: errors.New("injected recovery failure"),
	}
	repository.armed.Store(true)
	defer func() {
		close(repository.release)
		_ = a.Close()
		select {
		case <-repository.entered:
			select {
			case <-repository.done:
			case <-time.After(5 * time.Second):
				t.Error("test worker still running; retaining its resources")
				return
			}
		default:
		}
		closeRetainedTestResources(t, a)
	}()
	_, err = a.NewSupervisor(agentmanager.Config{Agents: repository, WaitPollInterval: time.Millisecond})
	if !errors.Is(err, repository.failure) || !errors.Is(err, agentmanager.ErrSupervisorCloseTimeout) {
		t.Errorf("failed assembly lost recovery/close errors: %v", err)
	}
	if _, ok := a.Registry().Snapshot().Get("agent_spawn"); ok {
		t.Error("failed recovery left its control bindings active")
	}
	if _, _, err := a.EnsureRootThread(context.Background(), "after-failed-assembly"); err == nil {
		t.Error("failed assembly with a live worker did not quarantine the App")
	}
	if _, err := a.NewSupervisor(agentmanager.Config{}); err == nil {
		t.Error("failed assembly admitted a replacement Supervisor")
	}
	closeErr := a.Close()
	if !errors.Is(closeErr, agentmanager.ErrSupervisorCloseTimeout) {
		t.Errorf("App lost failed Supervisor ownership: %v", closeErr)
	}
	assertTimedOutAppRetainsResources(t, a, closeErr)
}

func assertTimedOutAppRetainsResources(t *testing.T, a *App, closeErr error) {
	t.Helper()
	if err := a.Close(); err != closeErr {
		t.Errorf("repeated Close changed its result: %v -> %v", closeErr, err)
	}
	if err := requireOwned(a.config.SessionDatabasePath()); err != nil {
		t.Error(err)
	}
	if _, err := a.Orchestration().ListTeams(context.Background()); err != nil {
		t.Errorf("Close timeout closed a worker resource: %v", err)
	}
	if _, _, err := a.EnsureRootThread(context.Background(), "after-close"); err == nil {
		t.Error("closed App admitted new work")
	}
	if _, finish, err := a.beginEvolution(context.Background()); err == nil {
		finish()
		t.Error("closed App admitted evolution work")
	}
}

func requireOwned(databasePath string) error {
	lock, err := runtimeowner.Acquire(databasePath)
	if errors.Is(err, runtimeowner.ErrOwned) {
		return nil
	}
	_ = lock.Close()
	return fmt.Errorf("expected runtime ownership to remain held, got %v", err)
}

// Only tests may tear down retained resources, after their synthetic work no
// longer accesses them. Production timeout recovery requires process exit.
func closeRetainedTestResources(t *testing.T, a *App) {
	t.Helper()
	if err := a.plugins.Close(context.Background()); err != nil {
		t.Error(err)
	}
	closeOpenResources(a.database, a.wasm, a.approval, a.diagnostics)
	if err := a.owner.Close(); err != nil {
		t.Error(err)
	}
}

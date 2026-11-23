package atestore

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	ate "github.com/vkviyu/agent-temporal-engine"
	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/agent-temporal-engine/service"
	"github.com/vkviyu/agent-temporal-engine/spacecatalog"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	sessionate "github.com/vkviyu/go-self-evolving-agent/internal/session/atestore"
)

// Only immutable pending request envelopes use host SQLite. Every business
// record is read/written through the actual native ATE client and service.
type coreNativeProcess struct {
	engine *ate.Store
	lease  sync.Mutex
}

func (p *coreNativeProcess) WithClient(ctx context.Context, fn func(*client.Client) error) error {
	// Match the production process manager's serialized client leases.
	// Independent-writer CAS is tested separately by ateauthority.
	p.lease.Lock()
	defer p.lease.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	server, err := service.New(p.engine, service.Options{Credentials: map[string]service.Credential{"test": {Principal: "gsea", AllSpaces: true, AllowGlobalSnapshot: true}}})
	if err != nil {
		return err
	}
	defer server.Shutdown(context.Background())
	a, b := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- server.ServeStream(ctx, b) }()
	defer func() { _ = a.Close(); <-done }()
	c, err := client.New(ctx, a, client.Options{Token: "test"})
	if err != nil {
		return err
	}
	defer c.Close()
	return fn(c)
}

type coreFixture struct {
	store   *Store
	process *coreNativeProcess
	path    string
}

func newTestStore(t *testing.T) *Store { t.Helper(); return newCoreFixture(t).store }
func coreMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func newCoreFixture(t *testing.T) *coreFixture {
	t.Helper()
	ctx := context.Background()
	parent := t.TempDir()
	path := filepath.Join(parent, "native")
	coreMust(t, os.Mkdir(path, 0700))
	engine, err := ate.Create(path, ate.Options{AgentProfile: true, EventProfile: true})
	coreMust(t, err)
	p := &coreNativeProcess{engine: engine}
	t.Cleanup(func() { coreMust(t, p.engine.Close()) })
	created, err := engine.ApplySpace(ctx, api.ApplySpaceRequest{Key: model.RequestKey{Space: "sessions", Principal: "gsea", RequestID: "create-sessions"}, Command: spacecatalog.Apply{Name: "Sessions"}})
	coreMust(t, err)
	if created.Code != api.CodeDurableAck {
		t.Fatalf("create session space: %+v", created)
	}
	db, err := runtimestore.OpenRuntime(ctx, filepath.Join(parent, "pending.db"))
	coreMust(t, err)
	t.Cleanup(func() { coreMust(t, db.Close()) })
	authority, err := ateauthority.New(ateauthority.Options{Process: p, Pending: db.ATERequests(), Space: "orchestration", Principal: "gsea"})
	coreMust(t, err)
	snapshot, err := engine.Snapshot(ctx)
	coreMust(t, err)
	events, err := sessionate.New(sessionate.Options{Process: p, Pending: db.ATERequests(), Space: "sessions", StoreID: snapshot.Fence.Store})
	coreMust(t, err)
	store, err := New(authority, events)
	coreMust(t, err)
	return &coreFixture{store: store, process: p, path: path}
}
func (f *coreFixture) reopen(t *testing.T) {
	t.Helper()
	coreMust(t, f.process.engine.Close())
	engine, report, err := ate.OpenExisting(f.path, ate.Options{AgentProfile: true, EventProfile: true})
	coreMust(t, err)
	f.process.engine = engine
	if !report.Verified || !report.PublicationSynced {
		t.Fatal("native reopen did not verify publication")
	}
}
func coreRoot(t *testing.T, store *Store, id string) orchestration.RootThread {
	t.Helper()
	root, created, err := store.EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: session.ID(id)})
	coreMust(t, err)
	if !created {
		t.Fatal("expected new root")
	}
	return root
}
func coreChild(t *testing.T, store *Store, root orchestration.RootThread) orchestration.AgentSnapshot {
	t.Helper()
	child, err := store.CreateChild(context.Background(), orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker-v1", InitialPrompt: "retained prompt", IdempotencyKey: "child"})
	coreMust(t, err)
	return child
}
func TestCoreNativeRecordsReopenAndTeamEvents(t *testing.T) {
	f := newCoreFixture(t)
	s := f.store
	ctx := context.Background()
	root := coreRoot(t, s, "root/a")
	child := coreChild(t, s, root)
	other := coreRoot(t, s, "root/b")
	team, err := s.TransitionTeam(ctx, orchestration.TeamTransition{TeamID: root.TeamID, Expected: 1, Target: orchestration.TeamDraining})
	coreMust(t, err)
	_, err = s.TransitionTeam(ctx, orchestration.TeamTransition{TeamID: root.TeamID, Expected: team.Version, Target: orchestration.TeamCompleted})
	coreMust(t, err)
	f.reopen(t)
	got, err := s.GetAgent(ctx, child.ID)
	coreMust(t, err)
	if got.InitialPrompt != "retained prompt" {
		t.Fatalf("lost prompt: %+v", got)
	}
	reopened, created, err := s.EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: root.SessionID})
	coreMust(t, err)
	if created || reopened.TeamID != root.TeamID || reopened.TeamState != orchestration.TeamActive {
		t.Fatalf("reopen: %+v %v", reopened, created)
	}
	teams, err := s.ListTeams(ctx)
	coreMust(t, err)
	if len(teams) != 2 || teams[0] != root.TeamID || teams[1] != other.TeamID {
		t.Fatalf("team order: %v", teams)
	}
	events, err := s.LoadTeam(ctx, other.TeamID, 0, 1)
	coreMust(t, err)
	if len(events) != 1 || events[0].TeamID != other.TeamID {
		t.Fatalf("team filter before limit: %+v", events)
	}
	value, err := f.process.engine.GetState(ctx, api.ReadStateRequest{Space: "orchestration", Key: []byte(recordKey("agent", string(child.ID)))})
	coreMust(t, err)
	if !value.Found || value.Value.Kind != api.ValueBytes {
		t.Fatal("missing individual ATE agent record")
	}
	var stored orchestration.AgentSnapshot
	coreMust(t, json.Unmarshal(value.Value.Data, &stored))
	if stored.ID != child.ID {
		t.Fatal("wrong stored agent")
	}
}
func TestCoreSpawnClaimRollbackAndIdempotency(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	root := coreRoot(t, s, "claim")
	id, err := orchestration.NewTaskID()
	coreMust(t, err)
	coreMust(t, s.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		return createRecord(tx, "task", string(id), orchestration.TaskSnapshot{ID: id, TeamID: root.TeamID, State: orchestration.TaskReady, Version: 4})
	}))
	spec := orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "v1", TaskID: id, TaskVersion: 3, IdempotencyKey: "claim", CorrelationID: "first"}
	_, err = s.CreateChild(ctx, spec)
	if !errors.Is(err, orchestration.ErrTaskStateConflict) {
		t.Fatalf("stale dispatch: %v", err)
	}
	agents, err := s.ListAgents(ctx, orchestration.AgentQuery{TeamID: root.TeamID})
	coreMust(t, err)
	if len(agents) != 1 {
		t.Fatal("failed spawn leaked child")
	}
	spec.TaskVersion = 4
	child, err := s.CreateChild(ctx, spec)
	coreMust(t, err)
	task, err := s.GetTask(ctx, id)
	coreMust(t, err)
	if task.State != orchestration.TaskClaimed || task.AssigneeAgentID != child.ID || task.ClaimExpiresAt == nil {
		t.Fatalf("claim: %+v", task)
	}
	spec.CorrelationID = "retry"
	again, err := s.CreateChild(ctx, spec)
	coreMust(t, err)
	if again.ID != child.ID {
		t.Fatal("spawn retry duplicated child")
	}
	spec.InitialPrompt = "changed"
	_, err = s.CreateChild(ctx, spec)
	if !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("changed spawn accepted: %v", err)
	}
}
func TestCoreEventsCASAndRollback(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	root := coreRoot(t, s, "events")
	stream := orchestration.Stream{Type: orchestration.StreamTeam, ID: string(root.TeamID), TeamID: root.TeamID}
	before, err := s.LoadGlobal(ctx, 0, 100)
	coreMust(t, err)
	abort := errors.New("abort")
	err = s.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		if _, err := appendEvents(tx, stream, 1, orchestration.PendingEvent{Type: orchestration.EventTeamDraining, Payload: json.RawMessage(`{}`)}); err != nil {
			return err
		}
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Append(ctx, stream, 1, orchestration.PendingEvent{Type: orchestration.EventTeamDraining, Payload: json.RawMessage(`{}`)})
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, orchestration.ErrSequenceConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("CAS: %d success %d conflict", success, conflict)
	}
	after, err := s.LoadGlobal(ctx, 0, 100)
	coreMust(t, err)
	if len(after) != len(before)+1 || after[len(after)-1].GlobalSequence != uint64(len(after)) {
		t.Fatal("event rollback or counter gap")
	}
}

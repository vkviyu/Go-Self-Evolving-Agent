package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	orchestrationate "github.com/vkviyu/go-self-evolving-agent/internal/orchestration/atestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimeowner"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

// The nil embedded repository intentionally panics on any mutation. Attachment
// must use only authoritative reads, including for already canceled Teams.
type controlRootFixture struct {
	OrchestrationRepository
	root orchestration.RootThread
}

func (f controlRootFixture) GetTeam(context.Context, orchestration.TeamID) (orchestration.TeamSnapshot, error) {
	return orchestration.TeamSnapshot{ID: f.root.TeamID, RootAgentID: f.root.AgentID, State: orchestration.TeamCanceled}, nil
}
func (f controlRootFixture) GetAgent(context.Context, orchestration.AgentID) (orchestration.AgentSnapshot, error) {
	return orchestration.AgentSnapshot{ID: f.root.AgentID, TeamID: f.root.TeamID, SessionID: f.root.SessionID, ThreadID: f.root.ThreadID}, nil
}
func (f controlRootFixture) RootThreadBySession(context.Context, session.ID) (orchestration.RootThread, error) {
	return f.root, nil
}

func TestControlScopeAttachUsesAuthoritativeSessionLock(t *testing.T) {
	root := orchestration.RootThread{
		TeamID: "team_00000000000070008000000000000001", AgentID: "agent_00000000000070008000000000000001",
		ThreadID: "thread_00000000000070008000000000000001", SessionID: "root-a",
	}
	cfg := config.Config{ATE: config.ATEConfig{Directory: t.TempDir()}}
	makeApp := func() *App {
		a := &App{config: cfg, authority: &authorityResources{storeID: "shared-store"}, repositories: controlRootFixture{root: root}}
		t.Cleanup(func() { _ = a.Close() })
		return a
	}
	chat, control := makeApp(), makeApp()
	// Use the same lock primitive as EnsureRootThread without creating facts.
	if err := chat.ownSession(root.SessionID); err != nil {
		t.Fatal(err)
	}
	if err := control.AttachExistingTeam(t.Context(), root.TeamID); !errors.Is(err, runtimeowner.ErrOwned) {
		t.Fatalf("control acquired chat's Session: %v", err)
	}
	if len(control.ownedTeams) != 0 {
		t.Fatal("failed attachment granted Team ownership")
	}
	if err := chat.Close(); err != nil {
		t.Fatal(err)
	}
	if err := control.AttachExistingTeam(t.Context(), root.TeamID); err != nil {
		t.Fatal(err)
	}
	if _, ok := control.ownedTeams[root.TeamID]; !ok || len(control.ownedTeams) != 1 {
		t.Fatalf("wrong owned Teams: %v", control.ownedTeams)
	}
	if err := control.AttachExistingTeam(t.Context(), "team_00000000000070008000000000000002"); err == nil {
		t.Fatal("inconsistent root identity accepted")
	}
}

func TestControlScopeNativeReadsAndRecovery(t *testing.T) {
	ctx := t.Context()
	binary := filepath.Join(t.TempDir(), "ate")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "github.com/vkviyu/agent-temporal-engine/cmd/ate")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build ATE: %v\n%s", err, output)
	}
	cfg, err := (config.Config{
		HomeDirectory: t.TempDir(), DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir(),
		ATE: config.ATEConfig{Enabled: true, Binary: binary, Directory: filepath.Join(t.TempDir(), "ate"), Timeout: 30 * time.Second},
	}).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	db, err := runtimestore.OpenRuntime(ctx, filepath.Join(t.TempDir(), "seed.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	authority, err := openAuthority(ctx, cfg, db.ATERequests())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authority.process.Close() })
	store, err := orchestrationate.New(authority.records, authority.sessions)
	if err != nil {
		t.Fatal(err)
	}
	roots := make([]orchestration.RootThread, 2)
	requests := make([]tokenusage.ReservationRequest, 2)
	children := make([]orchestration.AgentSnapshot, 2)
	for i, id := range []session.ID{"control-owned", "control-foreign"} {
		roots[i], _, err = store.EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: id})
		if err != nil {
			t.Fatal(err)
		}
		root := roots[i]
		if err := store.ConfigureBudget(ctx, root.TeamID, tokenusage.BudgetLimits{MaxTotalTokens: 77}); err != nil {
			t.Fatal(err)
		}
		requests[i] = tokenusage.ReservationRequest{TeamID: root.TeamID, AgentID: root.AgentID, RequestID: "pending", TotalTokens: 10}
		if _, err := store.Reserve(ctx, requests[i]); err != nil {
			t.Fatal(err)
		}
		child, err := store.CreateChild(ctx, orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker-v1", InitialPrompt: "offline", IdempotencyKey: "child"})
		if err != nil {
			t.Fatal(err)
		}
		children[i], err = store.TransitionAgent(ctx, orchestration.AgentTransition{AgentID: child.ID, Expected: child.Version, Target: orchestration.AgentRunning})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := authority.process.Close(); err != nil {
		t.Fatal(err)
	}
	open := func() *App {
		t.Helper()
		a, err := OpenControl(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = a.Close() })
		return a
	}
	reader := open()
	before, err := reader.repositories.LoadGlobal(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range roots {
		snapshot, err := reader.LoadTeamSnapshot(ctx, root.TeamID)
		if err != nil || len(snapshot.Agents) != 2 {
			t.Fatalf("snapshot while another Team remains untouched: %+v, %v", snapshot, err)
		}
	}
	after, err := reader.repositories.LoadGlobal(ctx, 0, 1000)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("read-only control changed events: %v", err)
	}
	if reader.supervisor != nil || len(reader.sessionOwners) != 0 || reader.database != nil {
		t.Fatal("read-only control created an execution runtime")
	}
	if _, err := os.Stat(cfg.RuntimeDatabasePath()); !os.IsNotExist(err) {
		t.Fatalf("read-only control created runtime data: %v", err)
	}
	if err := reader.AttachExistingTeam(ctx, roots[0].TeamID); err != nil {
		t.Fatal(err)
	}
	supervisor, err := reader.NewControlSupervisor(ctx, roots[0].TeamID)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := reader.repositories.GetAgent(ctx, children[1].ID)
	if err != nil || !reflect.DeepEqual(foreign, children[1]) {
		t.Fatalf("foreign running child recovered: %+v, %v", foreign, err)
	}
	reserved, err := reader.repositories.Reserve(ctx, requests[1])
	if err != nil || reserved.State != tokenusage.ReservationPending {
		t.Fatalf("foreign reservation recovered: %+v, %v", reserved, err)
	}
	owned, err := reader.repositories.GetAgent(ctx, children[0].ID)
	if err != nil || owned.State != orchestration.AgentQueued {
		t.Fatalf("target child not recovered offline: %+v, %v", owned, err)
	}
	team, err := supervisor.CancelTeam(ctx, roots[0].TeamID, "control cancellation")
	if err != nil || team.State != orchestration.TeamCanceled {
		t.Fatalf("cancel: %+v, %v", team, err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	again := open()
	if err := again.AttachExistingTeam(ctx, roots[0].TeamID); err != nil {
		t.Fatal(err)
	}
	snapshot, err := again.LoadTeamSnapshot(ctx, roots[0].TeamID)
	if err != nil || snapshot.State != orchestration.TeamCanceled {
		t.Fatalf("attachment reopened canceled Team: %+v, %v", snapshot, err)
	}
	// No EnsureRootThread means the configured zero budget cannot replace 77.
	if _, err := again.NewControlSupervisor(ctx, roots[0].TeamID); err != nil {
		t.Fatal(err)
	}
	_, err = again.repositories.Reserve(ctx, tokenusage.ReservationRequest{
		TeamID: roots[0].TeamID, AgentID: roots[0].AgentID, RequestID: "over-budget", TotalTokens: 78,
	})
	if !errors.Is(err, tokenusage.ErrBudgetExceeded) {
		t.Fatalf("control reset Team budget: %v", err)
	}
	// A saved request has no generic proof of Team ownership. Leave it intact.
	record, err := aterequest.NewRecord(again.authority.storeID, api.CommitRequest{
		Space: authoritySpace, Principal: "gsea", RequestID: "foreign-pending", Method: api.MethodApplySpace, Body: []byte("{}"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := again.database.ATERequests().Insert(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
	blocked := open()
	if _, err := blocked.LoadTeamSnapshot(ctx, roots[0].TeamID); err != nil {
		t.Fatalf("pending request blocked a read: %v", err)
	}
	if err := blocked.AttachExistingTeam(ctx, roots[0].TeamID); err != nil {
		t.Fatal(err)
	}
	if _, err := blocked.NewControlSupervisor(ctx, roots[0].TeamID); err == nil {
		t.Fatal("control replayed an unscoped pending request")
	}
	saved, found, err := blocked.database.ATERequests().Get(ctx, record.Request.Key())
	if err != nil || !found || saved.Attempted {
		t.Fatalf("pending request was changed: %+v, %t, %v", saved, found, err)
	}
}

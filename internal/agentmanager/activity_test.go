package agentmanager

import (
	"context"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/sqlitestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

func TestLoadTeamSnapshotCombinesDurableProjections(t *testing.T) {
	database, err := sqlitestore.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: "activity-root"})
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := workspacecoord.NewCoordinator(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := NewSupervisor(Config{
		Agents: store, Tasks: store, Leases: newLeaseStore(t), Workspace: coordinator,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Close() })

	snapshot, err := supervisor.LoadTeamSnapshot(context.Background(), root.TeamID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.TeamID != root.TeamID {
		t.Fatalf("snapshot team = %q, want %q", snapshot.TeamID, root.TeamID)
	}
	if len(snapshot.Agents) != 1 || snapshot.Agents[0].ID != root.AgentID {
		t.Fatalf("unexpected agents: %#v", snapshot.Agents)
	}
	if snapshot.Tasks == nil || snapshot.Views == nil || snapshot.Leases == nil || snapshot.Merges == nil {
		t.Fatalf("snapshot projections must be initialized: %#v", snapshot)
	}
}

// fakeUsageReader reports a fixed total for every Agent it knows.
type fakeUsageReader struct {
	totals map[orchestration.AgentID]int
}

func (fakeUsageReader) RecordUsage(context.Context, tokenusage.Record) error { return nil }

func (fakeUsageReader) CanAdmit(context.Context, orchestration.TeamID) (bool, error) {
	return true, nil
}

func (reader fakeUsageReader) UsageReport(_ context.Context, query tokenusage.UsageQuery) (tokenusage.UsageReport, error) {
	return tokenusage.UsageReport{Query: query, Aggregate: tokenusage.UsageAggregate{TotalTokens: reader.totals[query.AgentID]}}, nil
}

func TestLoadTeamSnapshotOverlaysLiveActivityAndUsage(t *testing.T) {
	database, err := sqlitestore.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: "live-activity-root"})
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := workspacecoord.NewCoordinator(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	reported, release := make(chan struct{}), make(chan struct{})
	usage := fakeUsageReader{totals: map[orchestration.AgentID]int{}}
	supervisor, err := NewSupervisor(Config{
		Agents: store, Tasks: store, Mailbox: store, Leases: newLeaseStore(t), Workspace: coordinator, WorkspaceRoot: t.TempDir(), UsageLedger: usage,
		Runner: RunnerFunc(func(_ context.Context, request RunRequest) (orchestration.AgentResult, error) {
			request.Activity("tool read_file")
			close(reported)
			<-release
			return orchestration.AgentResult{Status: orchestration.ResultSucceeded, Summary: "done"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Close() })
	child, err := supervisor.Spawn(context.Background(), childSpec(root, "live", "inspect the parser"))
	if err != nil {
		t.Fatal(err)
	}
	usage.totals[child.ID] = 12500
	select {
	case <-reported:
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not report activity")
	}

	snapshot, err := supervisor.LoadTeamSnapshot(context.Background(), root.TeamID)
	if err != nil {
		t.Fatal(err)
	}
	if activity := snapshotActivity(snapshot, child.ID); activity != "tool read_file" {
		t.Fatalf("running child activity = %q", activity)
	}
	if snapshot.Usage[child.ID].TotalTokens != 12500 {
		t.Fatalf("child usage = %+v", snapshot.Usage)
	}

	close(release)
	result, err := supervisor.Wait(context.Background(), WaitCommand{AgentIDs: []orchestration.AgentID{child.ID}, Timeout: 5 * time.Second})
	if err != nil || !result.Satisfied {
		t.Fatalf("child did not finish: %+v, %v", result, err)
	}
	// A report after the run ended must not resurrect the live activity.
	supervisor.reportActivity(child.ID, "tool late")
	snapshot, err = supervisor.LoadTeamSnapshot(context.Background(), root.TeamID)
	if err != nil {
		t.Fatal(err)
	}
	if activity := snapshotActivity(snapshot, child.ID); activity == "tool read_file" || activity == "tool late" {
		t.Fatalf("finished child kept live activity %q", activity)
	}
}

// snapshotActivity returns the activity the snapshot shows for agentID.
func snapshotActivity(snapshot TeamSnapshot, agentID orchestration.AgentID) string {
	for _, agent := range snapshot.Agents {
		if agent.ID == agentID {
			return agent.Activity
		}
	}
	return ""
}

func TestLoadTeamSnapshotReturnsPartialDataWhenProjectionUnavailable(t *testing.T) {
	supervisor, _, root := newSupervisorTest(t, nil, 4, 3, 4)
	snapshot, err := supervisor.LoadTeamSnapshot(context.Background(), root.TeamID)
	if err == nil {
		t.Fatal("expected unavailable projection error")
	}
	if len(snapshot.Agents) != 1 || snapshot.Agents[0].ID != root.AgentID {
		t.Fatalf("agent projection was lost on partial failure: %#v", snapshot)
	}
}

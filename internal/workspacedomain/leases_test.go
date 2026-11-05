package workspacedomain

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

func TestLeasesConflictAcrossInstancesOfOneWorkspace(t *testing.T) {
	home, workspace := t.TempDir(), t.TempDir()
	first := openDomain(t, Options{HomeDirectory: home, WorkspaceRoot: workspace, DataDirectory: t.TempDir()})
	second := openDomain(t, Options{HomeDirectory: home, WorkspaceRoot: workspace, DataDirectory: t.TempDir()})
	firstTeam, firstAgent := newOwner(t)
	secondTeam, secondAgent := newOwner(t)
	ctx := context.Background()
	write := func(domain *Domain, team orchestration.TeamID, agent orchestration.AgentID, path, key string) (orchestration.WorkspaceLease, error) {
		return domain.Leases().AcquireLease(ctx, orchestration.LeaseSpec{TeamID: team, AgentID: agent, Path: path, Mode: orchestration.LeaseWrite, IdempotencyKey: key})
	}
	lease, err := write(first, firstTeam, firstAgent, "internal/app.go", "lease-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"internal", "internal/app.go", "INTERNAL/App.go", orchestration.WorkspaceRootLeasePath} {
		if _, err := write(second, secondTeam, secondAgent, path, ""); !errors.Is(err, orchestration.ErrLeaseConflict) {
			t.Fatalf("path %q: got %v, want cross-instance conflict", path, err)
		}
	}
	for _, path := range []string{"internal/app.go.orig", "internal_app.go", "foo..bar"} {
		unrelated, err := write(second, secondTeam, secondAgent, path, "")
		if err != nil {
			t.Fatalf("path %q was blocked: %v", path, err)
		}
		if _, err := second.Leases().ReleaseLease(ctx, unrelated.ID, secondAgent, "done"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := write(first, firstTeam, firstAgent, "other.txt", "lease-1"); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("got %v, want idempotency conflict", err)
	}
	read, err := second.Leases().AcquireLease(ctx, orchestration.LeaseSpec{TeamID: secondTeam, AgentID: secondAgent, Path: "README.md", Mode: orchestration.LeaseRead})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Leases().AcquireLease(ctx, orchestration.LeaseSpec{TeamID: firstTeam, AgentID: firstAgent, Path: "README.md", Mode: orchestration.LeaseRead}); err != nil {
		t.Fatalf("shared read was blocked: %v", err)
	}
	if _, err := first.Leases().ReleaseLease(ctx, read.ID, secondAgent, "done"); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("another live instance released the lease: %v", err)
	}
	if _, err := first.Leases().ReleaseLease(ctx, lease.ID, secondAgent, "done"); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("another Agent released the lease: %v", err)
	}
	released, err := first.Leases().ReleaseLease(ctx, lease.ID, firstAgent, "done")
	if err != nil || released.State != orchestration.LeaseReleased || released.ReleaseReason != "done" || released.ReleasedAt == nil {
		t.Fatalf("release = %#v, %v", released, err)
	}
	nested, err := write(second, secondTeam, secondAgent, "internal", "")
	if err != nil {
		t.Fatalf("released path remained blocked: %v", err)
	}
	if nested.Epoch <= lease.Epoch || nested.InstanceID != second.Status().InstanceID {
		t.Fatalf("epoch or holder not recorded: first=%#v nested=%#v", lease, nested)
	}
	all, err := first.Leases().ListLeases(ctx, orchestration.LeaseQuery{States: []orchestration.LeaseState{orchestration.LeaseActive}})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("workspace-wide listing = %#v", all)
	}
}

func TestLeasePathNormalizationMatchesValidationAndConflictScope(t *testing.T) {
	ctx := context.Background()
	domain := openDomain(t, Options{HomeDirectory: t.TempDir(), WorkspaceRoot: t.TempDir()})
	team, agent := newOwner(t)
	lease, err := domain.Leases().AcquireLease(ctx, orchestration.LeaseSpec{TeamID: team, AgentID: agent, Path: "  internal/./app.go \t", Mode: orchestration.LeaseWrite})
	if err != nil || lease.Path != "internal/app.go" {
		t.Fatalf("stored lease path = %q, err=%v", lease.Path, err)
	}
	if _, err := domain.Leases().AcquireLease(ctx, orchestration.LeaseSpec{TeamID: team, AgentID: agent, Path: "internal/app.go", Mode: orchestration.LeaseWrite}); !errors.Is(err, orchestration.ErrLeaseConflict) {
		t.Fatalf("normalized path did not conflict: %v", err)
	}
}

func TestLeaseWaitsForLiveHolderWithinBound(t *testing.T) {
	home, workspace := t.TempDir(), t.TempDir()
	holder := openDomain(t, Options{HomeDirectory: home, WorkspaceRoot: workspace})
	waiter := openDomain(t, Options{HomeDirectory: home, WorkspaceRoot: workspace})
	team, agent := newOwner(t)
	ctx := context.Background()
	held, err := holder.Leases().AcquireLease(ctx, orchestration.LeaseSpec{TeamID: team, AgentID: agent, Path: orchestration.WorkspaceRootLeasePath, Mode: orchestration.LeaseWrite})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := waiter.Leases().AcquireLease(ctx, orchestration.LeaseSpec{TeamID: team, AgentID: agent, Path: "a.txt", Mode: orchestration.LeaseWrite, Wait: 50 * time.Millisecond}); !errors.Is(err, orchestration.ErrLeaseConflict) {
		t.Fatalf("got %v, want conflict after bounded wait", err)
	}
	if elapsed := time.Since(started); elapsed < 50*time.Millisecond {
		t.Fatalf("conflict returned after %s, before the wait bound", elapsed)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		_, _ = holder.Leases().ReleaseLease(context.Background(), held.ID, agent, "done")
	}()
	if _, err := waiter.Leases().AcquireLease(ctx, orchestration.LeaseSpec{TeamID: team, AgentID: agent, Path: "a.txt", Mode: orchestration.LeaseWrite, Wait: 5 * time.Second}); err != nil {
		t.Fatalf("waiter was not admitted after release: %v", err)
	}
}

func TestLeasesOfExitedInstanceAreReclaimed(t *testing.T) {
	home, workspace := t.TempDir(), t.TempDir()
	exited := openDomain(t, Options{HomeDirectory: home, WorkspaceRoot: workspace})
	survivor := openDomain(t, Options{HomeDirectory: home, WorkspaceRoot: workspace})
	team, agent := newOwner(t)
	ctx := context.Background()
	stale, err := exited.Leases().AcquireLease(ctx, orchestration.LeaseSpec{TeamID: team, AgentID: agent, Path: "src", Mode: orchestration.LeaseWrite})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exited.Leases().AcquireLease(ctx, orchestration.LeaseSpec{TeamID: team, AgentID: agent, Path: "docs", Mode: orchestration.LeaseWrite}); err != nil {
		t.Fatal(err)
	}
	if count, err := survivor.Leases().ReapAbandonedLeases(ctx); err != nil || count != 0 {
		t.Fatalf("live holder was reaped: count=%d err=%v", count, err)
	}
	crash(exited)
	if _, err := survivor.Leases().AcquireLease(ctx, orchestration.LeaseSpec{TeamID: team, AgentID: agent, Path: "src/main.go", Mode: orchestration.LeaseWrite}); err != nil {
		t.Fatalf("lease of exited holder was not reclaimed: %v", err)
	}
	leases, err := survivor.Leases().ListLeases(ctx, orchestration.LeaseQuery{TeamID: team})
	if err != nil {
		t.Fatal(err)
	}
	abandoned := 0
	for _, lease := range leases {
		if lease.InstanceID == stale.InstanceID {
			if lease.State != orchestration.LeaseAbandoned || lease.ReleaseReason != abandonedReason {
				t.Fatalf("exited holder lease = %#v", lease)
			}
			abandoned++
		}
	}
	if abandoned != 2 {
		t.Fatalf("reclaimed %d leases of the exited instance, want 2: %#v", abandoned, leases)
	}
	if count, err := survivor.Leases().ReapAbandonedLeases(ctx); err != nil || count != 0 {
		t.Fatalf("reclaimed leases twice: count=%d err=%v", count, err)
	}
}

func TestReapAbandonedLeasesReclaimsEveryExitedInstance(t *testing.T) {
	home, workspace := t.TempDir(), t.TempDir()
	survivor := openDomain(t, Options{HomeDirectory: home, WorkspaceRoot: workspace})
	team, agent := newOwner(t)
	ctx := context.Background()
	for _, path := range []string{"a", "b"} {
		exited := openDomain(t, Options{HomeDirectory: home, WorkspaceRoot: workspace})
		if _, err := exited.Leases().AcquireLease(ctx, orchestration.LeaseSpec{TeamID: team, AgentID: agent, Path: path, Mode: orchestration.LeaseWrite}); err != nil {
			t.Fatal(err)
		}
		crash(exited)
	}
	if count, err := survivor.Leases().ReapAbandonedLeases(ctx); err != nil || count != 2 {
		t.Fatalf("count=%d err=%v, want both exited instances reclaimed", count, err)
	}
	active, err := survivor.Leases().ListLeases(ctx, orchestration.LeaseQuery{States: []orchestration.LeaseState{orchestration.LeaseActive}})
	if err != nil || len(active) != 0 {
		t.Fatalf("active leases after reclamation = %#v, %v", active, err)
	}
}

func TestDescendantKeepsExitedInstanceLeases(t *testing.T) {
	home, workspace := t.TempDir(), t.TempDir()
	parent := openDomain(t, Options{HomeDirectory: home, WorkspaceRoot: workspace})
	other := openDomain(t, Options{HomeDirectory: home, WorkspaceRoot: workspace})
	team, agent := newOwner(t)
	ctx := context.Background()
	if _, err := parent.Leases().AcquireLease(ctx, orchestration.LeaseSpec{TeamID: team, AgentID: agent, Path: orchestration.WorkspaceRootLeasePath, Mode: orchestration.LeaseWrite}); err != nil {
		t.Fatal(err)
	}
	token, err := parent.DescendantLock()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command("sleep", "30")
	child.ExtraFiles = []*os.File{token}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	_ = token.Close()
	crash(parent)
	if _, err := other.Leases().AcquireLease(ctx, orchestration.LeaseSpec{TeamID: team, AgentID: agent, Path: "file.txt", Mode: orchestration.LeaseWrite}); !errors.Is(err, orchestration.ErrLeaseConflict) {
		t.Fatalf("lease was reclaimed while a descendant survived: %v", err)
	}
	_ = child.Process.Kill()
	_ = child.Wait()
	if _, err := other.Leases().AcquireLease(ctx, orchestration.LeaseSpec{TeamID: team, AgentID: agent, Path: "file.txt", Mode: orchestration.LeaseWrite}); err != nil {
		t.Fatalf("lease was not reclaimed after the descendant exited: %v", err)
	}
}

func TestOwnerValidatorRejectsUnknownAgents(t *testing.T) {
	rejected := errors.New("unknown Agent")
	domain := openDomain(t, Options{
		HomeDirectory: t.TempDir(), WorkspaceRoot: t.TempDir(),
		ValidateOwner: func(context.Context, orchestration.TeamID, orchestration.AgentID) error { return rejected },
	})
	team, agent := newOwner(t)
	if _, err := domain.Leases().AcquireLease(context.Background(), orchestration.LeaseSpec{TeamID: team, AgentID: agent, Path: "a", Mode: orchestration.LeaseWrite}); !errors.Is(err, rejected) {
		t.Fatalf("got %v, want owner rejection", err)
	}
}

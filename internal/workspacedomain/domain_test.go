package workspacedomain

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

func TestIdentifySharesIdentityAcrossSymlinkAliases(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(workspace, alias); err != nil {
		t.Fatal(err)
	}
	direct, err := Identify(workspace)
	if err != nil {
		t.Fatal(err)
	}
	aliased, err := Identify(alias)
	if err != nil {
		t.Fatal(err)
	}
	if direct.ID != aliased.ID || direct.Root != aliased.Root {
		t.Fatalf("aliases produced different identities: %#v %#v", direct, aliased)
	}
	other, err := Identify(root)
	if err != nil {
		t.Fatal(err)
	}
	if other.ID == direct.ID {
		t.Fatal("distinct directories share an identity")
	}
	if _, err := Identify(filepath.Join(root, "missing")); err == nil {
		t.Fatal("identified a missing directory")
	}
}

func TestRendezvousRejectsAnotherHomeUntilMembersLeave(t *testing.T) {
	workspace, rendezvous := t.TempDir(), t.TempDir()
	firstHome, secondHome := t.TempDir(), t.TempDir()
	first := openDomain(t, Options{HomeDirectory: firstHome, RendezvousDirectory: rendezvous, WorkspaceRoot: workspace})
	if status := first.Status(); !status.CrossHomeExclusion || status.Degraded != "" {
		t.Fatalf("rendezvous was not joined: %#v", status)
	}
	sameHome := openDomain(t, Options{HomeDirectory: firstHome, RendezvousDirectory: rendezvous, WorkspaceRoot: workspace, DataDirectory: t.TempDir()})
	if _, err := Open(context.Background(), Options{HomeDirectory: secondHome, RendezvousDirectory: rendezvous, WorkspaceRoot: workspace}); !errors.Is(err, ErrForeignDomain) {
		t.Fatalf("foreign home was admitted: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), Options{HomeDirectory: secondHome, RendezvousDirectory: rendezvous, WorkspaceRoot: workspace}); !errors.Is(err, ErrForeignDomain) {
		t.Fatalf("foreign home was admitted while a member remained: %v", err)
	}
	if err := sameHome.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(rendezvous, first.Status().Identity.ID+".lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("last member left its lock file: %v", err)
	}
	openDomain(t, Options{HomeDirectory: secondHome, RendezvousDirectory: rendezvous, WorkspaceRoot: workspace})
}

func TestRendezvousOverwritesRecordOfCrashedMembers(t *testing.T) {
	workspace, rendezvous := t.TempDir(), t.TempDir()
	crashed := openDomain(t, Options{HomeDirectory: t.TempDir(), RendezvousDirectory: rendezvous, WorkspaceRoot: workspace})
	crash(crashed)
	replacement := openDomain(t, Options{HomeDirectory: t.TempDir(), RendezvousDirectory: rendezvous, WorkspaceRoot: workspace})
	if !replacement.Status().CrossHomeExclusion {
		t.Fatalf("replacement did not join: %#v", replacement.Status())
	}
}

func TestRendezvousDegradesWhenDirectoryIsUnusable(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	domain := openDomain(t, Options{HomeDirectory: t.TempDir(), RendezvousDirectory: filepath.Join(blocker, "coordination"), WorkspaceRoot: t.TempDir()})
	if status := domain.Status(); status.CrossHomeExclusion || status.Degraded == "" {
		t.Fatalf("unusable rendezvous was not reported: %#v", status)
	}
}

func TestConcurrentFirstOpensShareOneDatabase(t *testing.T) {
	home, workspace := t.TempDir(), t.TempDir()
	domains := make([]*Domain, 4)
	errs := make([]error, len(domains))
	var group sync.WaitGroup
	for index := range domains {
		group.Go(func() {
			domains[index], errs[index] = Open(context.Background(), Options{HomeDirectory: home, WorkspaceRoot: workspace})
		})
	}
	group.Wait()
	for index, domain := range domains {
		if errs[index] != nil {
			t.Fatal(errs[index])
		}
		t.Cleanup(func() { _ = domain.Close() })
		if domain.Status().Directory != domains[0].Status().Directory {
			t.Fatalf("instances opened different domains: %q %q", domain.Status().Directory, domains[0].Status().Directory)
		}
	}
}

func TestCloseReleasesLeasesAndUnregistersInstance(t *testing.T) {
	home, workspace := t.TempDir(), t.TempDir()
	closing := openDomain(t, Options{HomeDirectory: home, WorkspaceRoot: workspace})
	observer := openDomain(t, Options{HomeDirectory: home, WorkspaceRoot: workspace})
	team, agent := newOwner(t)
	lease, err := closing.Leases().AcquireLease(context.Background(), orchestration.LeaseSpec{TeamID: team, AgentID: agent, Path: "file.txt", Mode: orchestration.LeaseWrite})
	if err != nil {
		t.Fatal(err)
	}
	if err := closing.Close(); err != nil {
		t.Fatal(err)
	}
	leases, err := observer.Leases().ListLeases(context.Background(), orchestration.LeaseQuery{TeamID: team})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].ID != lease.ID || leases[0].State != orchestration.LeaseReleased || leases[0].ReleaseReason != "instance closed" {
		t.Fatalf("closed instance kept its lease: %#v", leases)
	}
	locks := filepath.Join(closing.Status().Directory, instancesDirectoryName)
	for _, path := range []string{processLockPath(locks, closing.Status().InstanceID), descendantLockPath(locks, closing.Status().InstanceID)} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("closed instance left %s: %v", path, err)
		}
	}
}

func TestLastCloseRemovesDomainDirectory(t *testing.T) {
	home, workspace := t.TempDir(), t.TempDir()
	first := openDomain(t, Options{HomeDirectory: home, WorkspaceRoot: workspace})
	second := openDomain(t, Options{HomeDirectory: home, WorkspaceRoot: workspace})
	directory := first.Status().Directory
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(directory, databaseName)); err != nil {
		t.Fatalf("directory removed while an instance remained: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("last instance left its domain directory: %v", err)
	}
	reopened := openDomain(t, Options{HomeDirectory: home, WorkspaceRoot: workspace})
	team, agent := newOwner(t)
	if _, err := reopened.Leases().AcquireLease(context.Background(), orchestration.LeaseSpec{TeamID: team, AgentID: agent, Path: "a", Mode: orchestration.LeaseWrite}); err != nil {
		t.Fatalf("recreated domain is unusable: %v", err)
	}
}

func TestCloseKeepsDirectoryOfCrashedInstanceWithLiveDescendant(t *testing.T) {
	home, workspace := t.TempDir(), t.TempDir()
	crashed := openDomain(t, Options{HomeDirectory: home, WorkspaceRoot: workspace})
	token, err := crashed.DescendantLock()
	if err != nil {
		t.Fatal(err)
	}
	crash(crashed)
	closing := openDomain(t, Options{HomeDirectory: home, WorkspaceRoot: workspace})
	if err := closing.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(crashed.Status().Directory, databaseName)); err != nil {
		t.Fatalf("directory removed while a descendant of a crashed instance was alive: %v", err)
	}
	// Once the descendant is gone, the next last instance cleans up.
	_ = token.Close()
	last := openDomain(t, Options{HomeDirectory: home, WorkspaceRoot: workspace})
	if err := last.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(crashed.Status().Directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("directory of exited instances was kept: %v", err)
	}
}

// openDomain opens a domain that the test closes during cleanup.
func openDomain(t *testing.T, options Options) *Domain {
	t.Helper()
	domain, err := Open(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = domain.Close() })
	return domain
}

// crash simulates process death: every descriptor is dropped and nothing is
// released or unregistered.
func crash(domain *Domain) {
	domain.closeOnce.Do(func() {
		_ = domain.db.Close()
		_ = domain.locks.process.Close()
		if domain.member != nil {
			_ = domain.member.file.Close()
		}
	})
}

// newOwner returns fresh Team and Agent identifiers for lease requests.
func newOwner(t *testing.T) (orchestration.TeamID, orchestration.AgentID) {
	t.Helper()
	team, err := orchestration.NewTeamID()
	if err != nil {
		t.Fatal(err)
	}
	agent, err := orchestration.NewAgentID()
	if err != nil {
		t.Fatal(err)
	}
	return team, agent
}

package app

import (
	"context"
	"errors"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacedomain"
)

func TestAppsShareWorkspaceLeasesAcrossDataDirectories(t *testing.T) {
	home, workspace, rendezvous := t.TempDir(), t.TempDir(), t.TempDir()
	open := func() *App {
		application, err := Open(context.Background(), config.Config{
			HomeDirectory: home, DataDirectory: t.TempDir(), WorkspaceDirectory: workspace, CoordinationDirectory: rendezvous,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = application.Close() })
		return application
	}
	first, second := open(), open()
	ctx := context.Background()
	firstRoot, _, err := first.EnsureRootThread(ctx, "first-session")
	if err != nil {
		t.Fatal(err)
	}
	secondRoot, _, err := second.EnsureRootThread(ctx, "second-session")
	if err != nil {
		t.Fatal(err)
	}
	write := func(application *App, root orchestration.RootThread, path string) error {
		_, err := application.domain.Leases().AcquireLease(ctx, orchestration.LeaseSpec{TeamID: root.TeamID, AgentID: root.AgentID, Path: path, Mode: orchestration.LeaseWrite})
		return err
	}
	if err := write(first, firstRoot, "main.go"); err != nil {
		t.Fatal(err)
	}
	if err := write(second, secondRoot, "main.go"); !errors.Is(err, orchestration.ErrLeaseConflict) {
		t.Fatalf("second Session wrote beside the first: %v", err)
	}
	if err := write(second, firstRoot, "other.go"); !errors.Is(err, orchestration.ErrNotFound) {
		t.Fatalf("lease granted to an Agent of another Session: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := write(second, secondRoot, "main.go"); err != nil {
		t.Fatalf("closed Session kept its lease: %v", err)
	}
}

func TestAppRejectsWorkspaceCoordinatedByAnotherHome(t *testing.T) {
	workspace, rendezvous := t.TempDir(), t.TempDir()
	open := func(home, data string) (*App, error) {
		return Open(context.Background(), config.Config{
			HomeDirectory: home, DataDirectory: data, WorkspaceDirectory: workspace, CoordinationDirectory: rendezvous,
		})
	}
	first, err := open(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	otherHome, otherData := t.TempDir(), t.TempDir()
	if rejected, err := open(otherHome, otherData); !errors.Is(err, workspacedomain.ErrForeignDomain) {
		if rejected != nil {
			_ = rejected.Close()
		}
		t.Fatalf("workspace was opened from another GSEA_HOME: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	// The rejected attempt released its runtime ownership on the failed path.
	admitted, err := open(otherHome, otherData)
	if err != nil {
		t.Fatalf("workspace stayed claimed after its last member closed: %v", err)
	}
	if err := admitted.Close(); err != nil {
		t.Fatal(err)
	}
}

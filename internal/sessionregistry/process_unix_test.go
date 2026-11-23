//go:build darwin || linux

package sessionregistry_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/sessionregistry"
)

func TestReconcileOnlyProvenDeadProcesses(t *testing.T) {
	ctx := context.Background()
	cfg := catalogConfig(t)
	root := cfg.HomeDirectory
	registry := openCatalog(t, cfg)
	// Waiting reaps the child: kill(pid, 0) must no longer count it as alive.
	child := exec.Command("/bin/sh", "-c", "exit 0")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		pid  int
		want sessionregistry.State
	}{
		{"dead", child.Process.Pid, sessionregistry.StateStopped},
		{"live", os.Getpid(), sessionregistry.StateRunning},
		{"unknown", 0, sessionregistry.StateRunning},
	} {
		item := sessionregistry.Session{ID: test.name, Name: test.name, Workspace: root, Runtime: filepath.Join(root, test.name), State: sessionregistry.StateRunning, PID: test.pid}
		if err := registry.Create(ctx, item); err != nil {
			t.Fatal(err)
		}
		got, err := registry.ReconcileStopped(ctx, item.ID)
		if err != nil || got.State != test.want {
			t.Fatalf("%s reconciled to %s: %v", test.name, got.State, err)
		}
	}
}

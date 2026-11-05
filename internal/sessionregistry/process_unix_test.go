//go:build darwin || linux

package sessionregistry

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestReconcileOnlyProvenDeadProcesses(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	registry, err := Open(ctx, filepath.Join(root, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
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
		want State
	}{
		{"dead", child.Process.Pid, StateStopped},
		{"live", os.Getpid(), StateRunning},
		{"unknown", 0, StateRunning},
	} {
		item := Session{ID: test.name, Name: test.name, Workspace: root, Runtime: filepath.Join(root, test.name), State: StateRunning, PID: test.pid}
		if err := registry.Create(ctx, item); err != nil {
			t.Fatal(err)
		}
		got, err := registry.ReconcileStopped(ctx, item.ID)
		if err != nil || got.State != test.want {
			t.Fatalf("%s reconciled to %s: %v", test.name, got.State, err)
		}
	}
}

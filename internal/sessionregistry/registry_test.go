package sessionregistry

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRegistryTracksSessionResourcesAndLifecycle(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	registry, err := Open(ctx, filepath.Join(root, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })

	item := Session{
		ID:            "sess-a",
		Name:          "runtime analysis",
		Workspace:     filepath.Join(root, "workspace"),
		Runtime:       filepath.Join(root, "sessions", "sess-a"),
		DataDirectory: filepath.Join(root, "sessions", "sess-a", "data"),
		TempDirectory: filepath.Join(root, "sessions", "sess-a", "tmp"),
		ToolState:     filepath.Join(root, "sessions", "sess-a", "tool-state"),
		Model:         "test-model",
		Provider:      "openai",
	}
	if err := registry.Create(ctx, item); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{item.Runtime, item.DataDirectory, item.TempDirectory, item.ToolState} {
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			t.Fatal(err)
		}
	}
	got, err := registry.Get(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != item.Name || got.Workspace != item.Workspace || got.State != StateCreated {
		t.Fatalf("session = %#v", got)
	}
	if err := registry.UpdateState(ctx, item.ID, StateRunning, 123); err != nil {
		t.Fatal(err)
	}
	got, err = registry.Get(ctx, item.ID)
	if err != nil || got.State != StateRunning || got.PID != 123 {
		t.Fatalf("running session = %#v, %v", got, err)
	}
	items, err := registry.List(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("sessions = %#v, %v", items, err)
	}
	if err := registry.UpdateState(ctx, item.ID, StateStopped, 0); err != nil {
		t.Fatal(err)
	}
	if err := registry.Remove(ctx, item.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Get(ctx, item.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing session error = %v", err)
	}
}

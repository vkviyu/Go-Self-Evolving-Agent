package sessionregistry

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMissingAndUnavailableRegistryArePredictable(t *testing.T) {
	ctx := context.Background()
	registry, err := Open(ctx, filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	if err := registry.UpdateState(ctx, "missing", StateStopped, 0); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := registry.Remove(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	var unavailable *Registry
	if _, err := unavailable.Get(ctx, "missing"); err == nil {
		t.Fatal("nil registry accepted")
	}
	if _, err := unavailable.List(ctx); err == nil {
		t.Fatal("nil registry accepted")
	}
}

func TestFailedRegistrationDoesNotCreateOrDeleteUserData(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	registry, err := Open(ctx, filepath.Join(root, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	item := Session{ID: "existing", Name: "existing", Workspace: root, Runtime: filepath.Join(root, "first")}
	if err := registry.Create(ctx, item); err != nil {
		t.Fatal(err)
	}
	newPath := filepath.Join(root, "duplicate-resources")
	item.Runtime = newPath
	if err := registry.Create(ctx, item); err == nil {
		t.Fatal("duplicate accepted")
	}
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Fatalf("duplicate left resources: %v", err)
	}
	blocker := filepath.Join(root, "user-file")
	if err := os.WriteFile(blocker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	item.ID = "new"
	item.TempDirectory = filepath.Join(blocker, "invalid")
	if err := registry.Create(ctx, item); err == nil {
		t.Fatal("invalid resources accepted")
	}
	if _, err := registry.Get(ctx, item.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Fatalf("failed registration left resources: %v", err)
	}
	if data, err := os.ReadFile(blocker); err != nil || string(data) != "keep" {
		t.Fatalf("user data changed: %q %v", data, err)
	}
}

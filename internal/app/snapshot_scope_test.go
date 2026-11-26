package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/sandbox"
)

func TestNativeSnapshotScopeBindsRuntimeEnvironment(t *testing.T) {
	cfg := config.Config{DataDirectory: t.TempDir()}
	otherDirectory := t.TempDir()
	for _, directory := range []string{cfg.DataDirectory, otherDirectory} {
		db := (config.Config{DataDirectory: directory}).RuntimeDatabasePath()
		if err := os.WriteFile(db, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	original, err := nativeSnapshotScope(cfg, "/workspace", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*config.Config, *string, *sandbox.Mode){
		func(_ *config.Config, root *string, _ *sandbox.Mode) { *root = "/other-workspace" },
		func(cfg *config.Config, _ *string, _ *sandbox.Mode) { cfg.DataDirectory = otherDirectory },
		func(_ *config.Config, _ *string, mode *sandbox.Mode) { *mode = sandbox.Mode("container") },
		func(cfg *config.Config, _ *string, _ *sandbox.Mode) { cfg.Model.BaseURL = "https://changed.invalid" },
	} {
		changed, root, mode := cfg, "/workspace", sandbox.Mode("")
		change(&changed, &root, &mode)
		actual, err := nativeSnapshotScope(changed, root, mode)
		if err != nil || actual == original {
			t.Fatalf("environment drift not identified: %s, %v", actual, err)
		}
	}
	alias := cfg
	alias.DataDirectory = filepath.Join(t.TempDir(), "state-link")
	if err := os.Symlink(cfg.DataDirectory, alias.DataDirectory); err != nil {
		t.Fatal(err)
	}
	aliased, err := nativeSnapshotScope(alias, "/workspace", "")
	if err != nil || aliased != original {
		t.Fatalf("runtime database alias changed scope: %q, %v", aliased, err)
	}
}

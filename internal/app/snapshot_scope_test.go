package app

import (
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/sandbox"
)

func TestNativeSnapshotScopeBindsRuntimeEnvironment(t *testing.T) {
	cfg := config.Config{DataDirectory: "/state"}
	original, err := nativeSnapshotScope(cfg, "/workspace", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*config.Config, *string, *sandbox.Mode){
		func(_ *config.Config, root *string, _ *sandbox.Mode) { *root = "/other-workspace" },
		func(cfg *config.Config, _ *string, _ *sandbox.Mode) { cfg.DataDirectory = "/other-state" },
		func(_ *config.Config, _ *string, mode *sandbox.Mode) { *mode = sandbox.Mode("container") },
		func(cfg *config.Config, _ *string, _ *sandbox.Mode) { cfg.Model.BaseURL = "https://changed.invalid" },
		func(cfg *config.Config, _ *string, _ *sandbox.Mode) { cfg.LSP.Enabled = true },
	} {
		changed, root, mode := cfg, "/workspace", sandbox.Mode("")
		change(&changed, &root, &mode)
		actual, err := nativeSnapshotScope(changed, root, mode)
		if err != nil || actual == original {
			t.Fatalf("environment drift not identified: %s, %v", actual, err)
		}
	}
}

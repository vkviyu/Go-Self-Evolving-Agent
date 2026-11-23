package app

import (
	"errors"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/plugin"
	"github.com/vkviyu/go-self-evolving-agent/internal/pluginbuild"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func TestPluginBuildRequiresExplicitGoRoot(t *testing.T) {
	t.Setenv(pluginbuild.EnvGoRoot, "")
	ctx := t.Context()
	application, err := testOpen(t, ctx, config.Config{
		DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	if report, err := application.Doctor(ctx); err != nil || report.Status != "ok" {
		t.Fatalf("doctor without plugin toolchain: status=%q err=%v", report.Status, err)
	}
	result, err := application.BuildPlugin(ctx, pluginbuild.Request{
		Manifest: plugin.Manifest{PluginID: "echo", Tool: tool.Descriptor{
			Name: "extension_echo", Revision: "v1", Description: "Echo JSON",
			RiskClass: tool.RiskPure, InputSchema: []byte(`{"type":"object"}`),
		}},
		Source: `package main
import "encoding/json"
func Invoke(input json.RawMessage) (json.RawMessage, error) { return input, nil }`,
		Cases: []plugin.TestCase{{Name: "echo", Input: []byte(`{}`), Expected: []byte(`{}`)}},
	})
	if !errors.Is(err, pluginbuild.ErrToolchainUnavailable) || result.State != "failed" {
		t.Fatalf("build without explicit toolchain: state=%q err=%v", result.State, err)
	}
}

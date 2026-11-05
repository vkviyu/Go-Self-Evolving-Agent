package sqlitestore

import (
	"context"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/plugin"
	"github.com/vkviyu/go-self-evolving-agent/internal/pluginbuild"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func TestBuildEvidenceInterruptedImmutableAndTampered(t *testing.T) {
	ctx := context.Background()
	db := abandonmentDatabase(t)
	request := pluginbuild.Request{Manifest: plugin.Manifest{PluginID: "build", Tool: tool.Descriptor{Name: "extension_test", Revision: "v1", Description: "test", RiskClass: tool.RiskPure, InputSchema: []byte(`{"type":"object"}`)}}, Source: "package main\nimport \"encoding/json\"\nfunc Invoke(input json.RawMessage)(json.RawMessage,error){return input,nil}", Cases: []plugin.TestCase{{Name: "echo", Input: []byte(`{}`), Expected: []byte(`{}`)}}}
	id, existing, err := db.BeginPluginBuild(ctx, request)
	if err != nil || existing != nil {
		t.Fatal("reserve", err)
	}
	if _, _, err := db.BeginPluginBuild(ctx, request); err == nil {
		t.Fatal("interrupted build replayed")
	}
	if _, result, err := db.PluginBuild(ctx, id); err != nil || result != nil {
		t.Fatal("running evidence not inspectable", err)
	}
	result := pluginbuild.Result{BuildID: id, State: "failed", Error: "diagnostic"}
	if err := db.FinishPluginBuild(ctx, result); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishPluginBuild(ctx, result); err == nil {
		t.Fatal("rewrote completed build")
	}
	if _, existing, err := db.BeginPluginBuild(ctx, request); err != nil || existing == nil || existing.Error != "diagnostic" {
		t.Fatal("lost failure receipt", err)
	}
	if _, err := db.db.Exec(`UPDATE plugin_builds SET result='{}' WHERE build_id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.BeginPluginBuild(ctx, request); err == nil {
		t.Fatal("replay accepted corrupt result")
	}
	if _, _, err := db.PluginBuild(ctx, id); err == nil {
		t.Fatal("inspection accepted corrupt result")
	}
	if _, err := db.db.Exec(`UPDATE plugin_builds SET request='{}' WHERE build_id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.PluginBuild(ctx, id); err == nil {
		t.Fatal("inspection accepted corrupt request")
	}
}

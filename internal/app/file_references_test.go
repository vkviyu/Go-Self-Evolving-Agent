package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/codingtools"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

func TestWorkspaceFileReferenceInspectorUsesActorViewAndReadPolicy(t *testing.T) {
	root := t.TempDir()
	worktree := filepath.Join(root, "worktree")
	if err := os.Mkdir(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	for directory, content := range map[string]string{root: "# Root\n", worktree: "# Worker\n一二\n"} {
		if err := os.WriteFile(filepath.Join(directory, "design.md"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	workspace, err := codingtools.New(root)
	if err != nil {
		t.Fatal(err)
	}
	team, _ := orchestration.NewTeamID()
	agent, _ := orchestration.NewAgentID()
	thread, _ := orchestration.NewThreadID()
	actor := orchestration.ActorScope{TeamID: team, AgentID: agent, ThreadID: thread}
	workspace.AttachWorkspaceViewRepository(referenceTestWorkspaceViews{actor: actor, root: worktree})
	const nativeScope = "metadata-test"
	registry := tool.NewRegistry()
	snapshot, err := registry.ActivateMany(scopeNativeBindings(nativeScope, workspace.Bindings())...)
	if err != nil {
		t.Fatal(err)
	}
	snapshotID, err := snapshot.ID()
	if err != nil {
		t.Fatal(err)
	}
	call := tool.Call{Actor: actor, SessionID: "child", TurnID: "turn", CallID: "metadata", ToolName: "workspace_read", Registry: snapshot, RuntimeSnapshotID: snapshotID}
	inspect := workspaceFileReferenceInspector(workspace, nativeScope)
	for _, effect := range []policy.Effect{policy.EffectAllow, policy.EffectDeny, policy.EffectRequireApproval} {
		t.Run(string(effect), func(t *testing.T) {
			var engine policy.Engine
			var err error
			if effect == policy.EffectAllow {
				engine, err = buildPolicy(nil)
			} else {
				engine, err = policy.NewStaticEngine(effect)
			}
			if err != nil {
				t.Fatal(err)
			}
			pipeline, err := tool.NewPipeline(tool.PipelineConfig{Registry: registry, Policy: engine, Events: memorystore.New()})
			if err != nil {
				t.Fatal(err)
			}
			files, err := inspect(t.Context(), pipeline, call, "Review @design.md")
			if err != nil {
				t.Fatal(err)
			}
			if effect != policy.EffectAllow {
				if len(files) != 0 {
					t.Fatalf("policy %s returned metadata: %#v", effect, files)
				}
				return
			}
			if len(files) != 1 || files[0].Path != "design.md" || len(files[0].Headings) != 1 || files[0].Headings[0].Title != "Worker" {
				t.Fatalf("expected worker workspace metadata, got %#v", files)
			}
		})
	}
}

type referenceTestWorkspaceViews struct {
	workspacecoord.Repository
	actor orchestration.ActorScope
	root  string
}

func (views referenceTestWorkspaceViews) ListViews(_ context.Context, query workspacecoord.ViewQuery) ([]workspacecoord.View, error) {
	if query.TeamID != views.actor.TeamID || query.AgentID != views.actor.AgentID {
		return nil, nil
	}
	return []workspacecoord.View{{RootPath: views.root, Mode: workspacecoord.ViewGitWorktree, State: workspacecoord.ViewActive}}, nil
}

package plugin_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/app"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

type snapshotFinalModel struct{ calls atomic.Int32 }

func (fake *snapshotFinalModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	fake.calls.Add(1)
	return schema.AssistantMessage("durable candidate", nil), nil
}

func (fake *snapshotFinalModel) Stream(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := fake.Generate(ctx, input, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func TestSnapshotRecoveryRestoresRetiredWASMWithoutGlobalRollback(t *testing.T) {
	if testing.Short() {
		t.Skip("real Go WASM restart integration")
	}
	ctx := context.Background()
	source := durableFixture(t)
	cfg := config.Config{DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir()}
	open := func(cfg config.Config) *app.App {
		application, err := app.Open(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = application.Close() })
		return application
	}
	application := open(cfg)
	if _, err := application.Plugins().Activate(stageDurable(t, application.Plugins(), "v1", source)); err != nil {
		t.Fatal(err)
	}
	fake := &snapshotFinalModel{}
	factory, err := application.NewAgentFactory(fake, agentruntime.Config{})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := factory.NewTurn(ctx, "snapshot-session")
	if err != nil {
		t.Fatal(err)
	}
	result, err := turn.Run(ctx, []*schema.Message{schema.UserMessage("remember this result")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := application.Plugins().Activate(stageDurable(t, application.Plugins(), "v2", source)); err != nil {
		t.Fatal(err)
	}
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
	application = open(cfg)
	current := application.Registry().Snapshot()
	if _, err := application.Registry().SnapshotByID(result.RuntimeSnapshotID); !errors.Is(err, tool.ErrSnapshotUnavailable) {
		t.Fatalf("old combination was eagerly published: %v", err)
	}
	factory, err = application.NewAgentFactory(fake, agentruntime.Config{})
	if err != nil {
		t.Fatal(err)
	}
	recovered, exists, err := factory.CompletedTurn(ctx, "snapshot-session", turn.ID())
	if err != nil || !exists || recovered.Final.Content != "durable candidate" || fake.calls.Load() != 1 {
		t.Fatalf("completed recovery=%+v exists=%t calls=%d err=%v", recovered, exists, fake.calls.Load(), err)
	}
	old, err := application.Registry().ResolveSnapshot(ctx, result.RuntimeSnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if application.Registry().Snapshot() != current || !application.Registry().OwnsSnapshot(old) {
		t.Fatal("recovery changed global active snapshot")
	}
	invokeDurable(t, old, "v1")
	invokeDurable(t, current, "v2")
	refs, err := application.RuntimeSnapshotReferences(ctx, result.RuntimeSnapshotID)
	if err != nil || len(refs) != 1 || refs[0] != (tool.SnapshotReference{Kind: "turn", Scope: "snapshot-session", ID: turn.ID()}) {
		t.Fatalf("refs=%+v err=%v", refs, err)
	}
	_, err = application.Pipeline().Execute(ctx, tool.Call{SessionID: "snapshot-session", TurnID: turn.ID(), CallID: "policy-still-applies", Registry: old, RuntimeSnapshotID: result.RuntimeSnapshotID, ToolName: "echo", Arguments: []byte(`{}`)})
	if !errors.Is(err, tool.ErrPolicyDenied) {
		t.Fatalf("restoration granted invocation authority: %v", err)
	}
	if _, err := application.Plugins().Retire("echo", "v2"); err != nil {
		t.Fatal(err)
	}
	if application.Registry().Snapshot().Generation() <= old.Generation() {
		t.Fatal("retirement overwrote an imported generation")
	}
	invokeDurable(t, old, "v1")
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
	changed := cfg
	changed.WorkspaceDirectory = t.TempDir()
	application = open(changed)
	if _, err := application.Registry().ResolveSnapshot(ctx, result.RuntimeSnapshotID); !errors.Is(err, tool.ErrSnapshotUnavailable) {
		t.Fatalf("recovery redirected native tools into another workspace: %v", err)
	}
}

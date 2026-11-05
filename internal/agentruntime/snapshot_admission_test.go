package agentruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

type unavailableSnapshotStore struct{ saves int }

func (store *unavailableSnapshotStore) Save(context.Context, tool.SnapshotManifest, tool.SnapshotReference) error {
	store.saves++
	return errors.New("snapshot admission failed")
}
func (*unavailableSnapshotStore) Load(context.Context, string) (tool.SnapshotManifest, error) {
	return tool.SnapshotManifest{}, tool.ErrSnapshotUnavailable
}

func TestNewTurnCannotStartWithoutDurableSnapshot(t *testing.T) {
	ctx := context.Background()
	events := memorystore.New()
	registry := tool.NewRegistry()
	store := &unavailableSnapshotStore{}
	if err := registry.ConfigurePersistence(store, nil); err != nil {
		t.Fatal(err)
	}
	engine, err := policy.DefaultDeny()
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := tool.NewPipeline(tool.PipelineConfig{Registry: registry, Policy: engine, Events: events})
	if err != nil {
		t.Fatal(err)
	}
	fake := &scriptedModel{}
	factory, err := NewFactory(Config{}, fake, registry, pipeline, events, events)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := factory.NewTurn(ctx, "no-snapshot"); err == nil {
		t.Fatal("admitted turn without snapshot")
	}
	sequence, err := events.CurrentSequence(ctx, "no-snapshot")
	if err != nil || sequence != 0 || fake.calls != 0 || store.saves != 1 {
		t.Fatalf("dispatched before persistence: sequence=%d calls=%d saves=%d err=%v", sequence, fake.calls, store.saves, err)
	}
}

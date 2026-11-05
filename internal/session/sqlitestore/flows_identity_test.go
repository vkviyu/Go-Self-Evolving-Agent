package sqlitestore

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentflow"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func TestFlowSnapshotIdentitySurvivesReopenAndGenerationChanges(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "flow.db")
	database, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	root, _, err := database.Orchestration().EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "flow-identity"})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := tool.NewRegistry().Snapshot().ID()
	if err != nil {
		t.Fatal(err)
	}
	input, _ := agentflow.NewValue("task")
	id, _ := agentflow.NewID()
	request := agentflow.RunRequest{ID: id, IdempotencyKey: "resume", Definition: "identity", DefinitionRevision: "v1", DefinitionFingerprint: strings.Repeat("a", 64), SessionID: root.SessionID, TeamID: root.TeamID, AgentID: root.AgentID, Input: input, RegistryGeneration: 1, RuntimeSnapshotID: identity}
	first, err := database.Orchestration().CreateRun(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	request.RegistryGeneration = 99
	request.ID, _ = agentflow.NewID()
	replayed, err := reopened.Orchestration().CreateRun(ctx, request)
	if err != nil || replayed.ID != first.ID || replayed.RuntimeSnapshotID != identity {
		t.Fatalf("identity not durable: %#v %v", replayed, err)
	}
	request.RuntimeSnapshotID += "changed"
	if _, err := reopened.Orchestration().CreateRun(ctx, request); !errors.Is(err, orchestration.ErrIdempotencyConflict) {
		t.Fatalf("changed identity accepted: %v", err)
	}
}

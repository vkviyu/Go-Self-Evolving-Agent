package codingtools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/sandbox"
	"github.com/vkviyu/go-self-evolving-agent/internal/testfixture"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

func TestWorkspaceRoutesSnapshotViewOutsideTheRoot(t *testing.T) {
	ctx := context.Background()
	workspaceRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceRoot, "a.txt"), []byte("main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	viewsDirectory := filepath.Join(t.TempDir(), "views")
	workRoot := filepath.Join(viewsDirectory, "snap-test", "work")
	if err := os.MkdirAll(workRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workRoot, "a.txt"), []byte("snapshot\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	database, err := testfixture.Open(t, ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	rootThread, _, err := store.EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "snapshot-view-root"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.CreateChild(ctx, orchestration.SpawnSpec{TeamID: rootThread.TeamID, ParentAgentID: rootThread.AgentID, ProfileRevision: "worker-v1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateView(ctx, workspacecoord.ViewSpec{
		TeamID: rootThread.TeamID, AgentID: child.ID, Mode: workspacecoord.ViewSnapshot,
		WorkspaceRoot: workspaceRoot, RootPath: workRoot, BaseRevision: "sha256:base", IdempotencyKey: "snapshot-routing",
	}); err != nil {
		t.Fatal(err)
	}
	workspace, err := New(workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	workspace.AttachWorkspaceViewRepository(store)
	actorContext := orchestration.WithActorScope(ctx, child.ActorScope())

	// Without the views directory the snapshot root is refused.
	if _, err := workspace.read(actorContext, json.RawMessage(`{"path":"a.txt"}`)); err == nil || !strings.Contains(err.Error(), "snapshot views directory") {
		t.Fatalf("read without views directory error = %v", err)
	}
	if err := workspace.AttachSnapshotViewsDirectory(viewsDirectory); err != nil {
		t.Fatal(err)
	}
	metadata, err := workspace.InspectFileReferences(actorContext, "review @a.txt", nil)
	if err != nil || len(metadata) != 1 || metadata[0].SizeBytes != int64(len("snapshot\n")) {
		t.Fatalf("snapshot file metadata = %#v, error = %v", metadata, err)
	}
	result, err := workspace.read(actorContext, json.RawMessage(`{"path":"a.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(result, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Content != "snapshot\n" {
		t.Fatalf("snapshot read = %q", decoded.Content)
	}

	// Writes stay in the work tree and need no lease.
	if _, err := workspace.write(actorContext, json.RawMessage(`{"path":"a.txt","content":"edited\n"}`)); err != nil {
		t.Fatal(err)
	}
	if content, _ := os.ReadFile(filepath.Join(workRoot, "a.txt")); string(content) != "edited\n" {
		t.Fatalf("snapshot content = %q", content)
	}
	if content, _ := os.ReadFile(filepath.Join(workspaceRoot, "a.txt")); string(content) != "main\n" {
		t.Fatalf("main content = %q", content)
	}

	// The shell runs in a sandbox rooted at the work tree.
	canonicalWork, err := filepath.EvalSymlinks(workRoot)
	if err != nil {
		t.Fatal(err)
	}
	recorder := &scopeRecordingRunner{}
	workspace.viewRunners = map[string]sandbox.Runner{canonicalWork: recorder}
	if _, err := workspace.shell(actorContext, json.RawMessage(`{"command":"true"}`)); err != nil {
		t.Fatal(err)
	}
	if recorder.request.WorkspaceRoot != canonicalWork || recorder.request.WorkingDirectory != canonicalWork {
		t.Fatalf("shell boundary = %#v, want %s", recorder.request, canonicalWork)
	}
	if !workspace.insideSnapshotViews(canonicalWork) || workspace.passiveDiagnostics(canonicalWork) {
		t.Fatal("snapshot work tree must skip passive diagnostics")
	}
}

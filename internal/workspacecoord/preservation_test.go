package workspacecoord_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/artifact"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/testfixture"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

func preservationFixture(t *testing.T) (*workspacecoord.Coordinator, workspacecoord.View) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, root, "init", "-q")
	fixtureGit(t, root, "config", "user.name", "Test")
	fixtureGit(t, root, "config", "user.email", "test@example.invalid")
	fixtureWrite(t, filepath.Join(root, "tracked.txt"), []byte("base\n"))
	fixtureWrite(t, filepath.Join(root, "deleted.txt"), []byte("delete me\n"))
	fixtureWrite(t, filepath.Join(root, ".gitignore"), []byte("/.gsea/\n/ignored.bin\n"))
	fixtureGit(t, root, "add", ".")
	fixtureGit(t, root, "commit", "-qm", "base")
	database, err := testfixture.Open(t, context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	thread, _, err := store.EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: "preservation"})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := workspacecoord.NewWorktreeManager(root)
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := workspacecoord.NewCoordinator(store, manager)
	if err != nil {
		t.Fatal(err)
	}
	coordinator.Artifacts, err = artifact.NewStore(filepath.Join(root, ".gsea", "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	coordinator.Events = store
	base, err := manager.ResolveHead(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	view, err := coordinator.CreateView(context.Background(), workspacecoord.ViewSpec{
		TeamID: thread.TeamID, AgentID: thread.AgentID, Mode: workspacecoord.ViewGitWorktree,
		WorkspaceRoot: root, RootPath: filepath.Join(manager.Worktrees, "worker"), BaseRevision: base,
	})
	if err != nil {
		t.Fatal(err)
	}
	return coordinator, view
}

func TestWorktreePatchIncludesNewBinaryAndDeletedFilesWithoutChangingIndex(t *testing.T) {
	coordinator, view := preservationFixture(t)
	fixtureWrite(t, filepath.Join(view.RootPath, "tracked.txt"), []byte("staged version\n"))
	fixtureGit(t, view.RootPath, "add", "tracked.txt")
	fixtureWrite(t, filepath.Join(view.RootPath, "tracked.txt"), []byte("working version\n"))
	fixtureWrite(t, filepath.Join(view.RootPath, "new.go"), []byte("package example\n"))
	fixtureWrite(t, filepath.Join(view.RootPath, "binary.dat"), []byte{0, 1, 2, 0, 255})
	fixtureWrite(t, filepath.Join(view.RootPath, "ignored.bin"), []byte("not in patch"))
	if err := os.Remove(filepath.Join(view.RootPath, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	indexPath := strings.TrimSpace(string(fixtureGit(t, view.RootPath, "rev-parse", "--git-path", "index")))
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	patch, err := coordinator.Worktrees.Diff(context.Background(), view)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(indexPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("worker index changed: %v", err)
	}
	for _, marker := range []string{"working version", "new.go", "GIT binary patch", "deleted file mode"} {
		if !bytes.Contains(patch, []byte(marker)) {
			t.Errorf("missing patch marker %q: %s", marker, patch)
		}
	}
	if bytes.Contains(patch, []byte("ignored.bin")) {
		t.Fatal("ignored output unexpectedly published")
	}
	if err := coordinator.Worktrees.ApplyPatch(context.Background(), patch); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(view.WorkspaceRoot, "binary.dat"))
	if err != nil || !bytes.Equal(data, []byte{0, 1, 2, 0, 255}) {
		t.Fatalf("binary roundtrip=%v err=%v", data, err)
	}
	if err := coordinator.ReleaseView(context.Background(), view.ID, "done"); err == nil {
		t.Fatal("dirty view was automatically discarded")
	}
	if _, err := os.Stat(filepath.Join(view.RootPath, "ignored.bin")); err != nil {
		t.Fatal(err)
	}
}

func TestFinalizePreservesPatchAndDurableReferenceIdempotently(t *testing.T) {
	coordinator, view := preservationFixture(t)
	fixtureWrite(t, filepath.Join(view.RootPath, "new.go"), []byte("package kept\n"))
	for range 2 {
		// A new coordinator has no process-local preservation state.
		fresh, err := workspacecoord.NewCoordinator(coordinator.Repository, coordinator.Worktrees)
		if err != nil {
			t.Fatal(err)
		}
		fresh.Artifacts, fresh.Events = coordinator.Artifacts, coordinator.Events
		if err := fresh.FinalizeView(context.Background(), view.ID, "finished"); err != nil {
			t.Fatal(err)
		}
	}
	records, err := coordinator.Events.LoadStream(context.Background(), orchestration.StreamWorkspace, string(view.TeamID), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range records {
		if event.Type != orchestration.EventWorkspaceViewPreserved {
			continue
		}
		count++
		var payload struct {
			Reference string `json:"patch_artifact"`
			Root      string `json:"root_path"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		patch, err := coordinator.Artifacts.(workspacecoord.ArtifactReader).ReadArtifact(context.Background(), payload.Reference)
		if err != nil || !bytes.Contains(patch, []byte("new.go")) || payload.Root != view.RootPath {
			t.Fatalf("artifact=%s err=%v", patch, err)
		}
	}
	if count != 1 {
		t.Fatalf("preservation events=%d", count)
	}
	stored, err := coordinator.Repository.GetView(context.Background(), view.ID)
	if err != nil || stored.State != workspacecoord.ViewActive {
		t.Fatalf("retained view=%#v err=%v", stored, err)
	}
	merges, err := coordinator.Repository.ListMerges(context.Background(), workspacecoord.MergeQuery{TeamID: view.TeamID})
	if err != nil || len(merges) != 0 {
		t.Fatalf("finalization implicitly authorized merge: %#v err=%v", merges, err)
	}
	if _, err := os.Stat(filepath.Join(view.RootPath, "new.go")); err != nil {
		t.Fatal(err)
	}
}

type failingPatchWriter struct{}

func TestMergePreservesOriginalWorktreeWithoutReportingCleanupFailure(t *testing.T) {
	coordinator, view := preservationFixture(t)
	fixtureWrite(t, filepath.Join(view.RootPath, "new.go"), []byte("package merged\n"))
	patch, err := coordinator.Worktrees.Diff(context.Background(), view)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := coordinator.Artifacts.Put(context.Background(), patch)
	if err != nil {
		t.Fatal(err)
	}
	_, err = coordinator.Repository.EnqueueMerge(context.Background(), workspacecoord.MergeSpec{
		TeamID: view.TeamID, AgentID: view.AgentID, ViewID: view.ID, BaseRevision: view.BaseRevision, PatchArtifact: reference,
	})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := workspacecoord.NewMergeExecutor(coordinator.Repository, coordinator, coordinator.Worktrees, coordinator.Artifacts.(workspacecoord.ArtifactReader), nil)
	if err != nil {
		t.Fatal(err)
	}
	executor.AutoMerge = true
	results, err := executor.RunPending(context.Background(), view.TeamID, 1)
	if err != nil || len(results) != 1 || results[0].State != workspacecoord.MergeApplied {
		t.Fatalf("merge=%#v err=%v", results, err)
	}
	for _, root := range []string{view.WorkspaceRoot, view.RootPath} {
		if _, err := os.Stat(filepath.Join(root, "new.go")); err != nil {
			t.Fatal(err)
		}
	}
}

func (failingPatchWriter) Put(context.Context, []byte) (string, error) {
	return "", errors.New("disk unavailable")
}

func TestPreservationFailureAndCreationCollisionNeverRemoveFiles(t *testing.T) {
	coordinator, view := preservationFixture(t)
	path := filepath.Join(view.RootPath, "keep.txt")
	fixtureWrite(t, path, []byte("keep"))
	coordinator.Artifacts = failingPatchWriter{}
	if err := coordinator.FinalizeView(context.Background(), view.ID, "failed"); err == nil {
		t.Fatal("storage failure ignored")
	}
	_, err := coordinator.CreateView(context.Background(), workspacecoord.ViewSpec{
		TeamID: view.TeamID, AgentID: view.AgentID, Mode: view.Mode, WorkspaceRoot: view.WorkspaceRoot, RootPath: view.RootPath, BaseRevision: view.BaseRevision,
	})
	if err == nil {
		t.Fatal("creation overwrote an existing worktree")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "keep" {
		t.Fatalf("output lost: %q err=%v", data, err)
	}
}

func TestOnlyPristineWorktreesCanBeReleased(t *testing.T) {
	t.Run("pristine", func(t *testing.T) {
		coordinator, view := preservationFixture(t)
		if err := coordinator.ReleaseView(context.Background(), view.ID, "unused"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(view.RootPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("pristine view not removed: %v", err)
		}
	})
	t.Run("ignored output", func(t *testing.T) {
		coordinator, view := preservationFixture(t)
		fixtureWrite(t, filepath.Join(view.RootPath, "ignored.bin"), []byte("keep"))
		if err := coordinator.ReleaseView(context.Background(), view.ID, "done"); err == nil {
			t.Fatal("ignored output discarded")
		}
	})
}

func fixtureWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func fixtureGit(t *testing.T, root string, arguments ...string) []byte {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = root
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
	return output
}

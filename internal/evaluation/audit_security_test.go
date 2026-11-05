package evaluation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPatchIgnoresAgentControlledGitMetadata(t *testing.T) {
	repository, base := newEvaluationRepository(t)
	worktree, err := PrepareWorktree(context.Background(), t.TempDir(), Task{ID: "git-security", Repository: repository, BaseCommit: base})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = worktree.Remove(context.Background()) })
	// Replace the view's gitfile with a malicious private repository. Host
	// capture must never consult it, even to find the actual Git directory.
	if err := os.Remove(filepath.Join(worktree.Path, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(worktree.Path, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "executed")
	config := "[core]\nrepositoryformatversion = 0\nfsmonitor = \"touch " + marker + "\"\n"
	if err := os.WriteFile(filepath.Join(worktree.Path, ".git", "config"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree.Path, "tracked.txt"), []byte("changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	patch, err := worktree.Patch(context.Background())
	if err != nil || !strings.Contains(string(patch), "+changed") {
		t.Fatalf("patch=%s err=%v", patch, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("Agent-controlled git command executed: %v", err)
	}
}

func TestDirectoryCaptureOwnsItsBaselineObjects(t *testing.T) {
	fixture := t.TempDir()
	if err := os.WriteFile(filepath.Join(fixture, "tracked.txt"), []byte("baseline\n"), 0600); err != nil {
		t.Fatal(err)
	}
	worktree, err := PrepareWorkspace(context.Background(), t.TempDir(), Task{ID: "copy-security", WorkspaceMode: WorkspaceDirectory, Repository: fixture})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = worktree.Remove(context.Background()) })
	// A directory-copy Agent owns this whole .git directory. The capture
	// baseline must not depend on it, including through Git alternates.
	objects := filepath.Join(worktree.Path, ".git", "objects")
	if err := os.RemoveAll(objects); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree.Path, "tracked.txt"), []byte("changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	patch, err := worktree.Patch(context.Background())
	if err != nil || !strings.Contains(string(patch), "-baseline") || !strings.Contains(string(patch), "+changed") {
		t.Fatalf("baseline lost: patch=%s err=%v", patch, err)
	}
}

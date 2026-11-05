package workspacecoord_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPatchCaptureNeverExecutesAgentConfiguredGitFilters(t *testing.T) {
	for _, kind := range []string{"clean", "process"} {
		t.Run(kind, func(t *testing.T) {
			coordinator, view := preservationFixture(t)
			outside := t.TempDir()
			marker := filepath.Join(outside, "host-filter-executed")
			t.Setenv("DATABASE_URL", "postgres://fake-audit-secret@invalid/database")
			// Configuring a filter is harmless here. In the vulnerable code the
			// later automatic git add executes it in the host, outside the view.
			filter := "printf '%s' \"$DATABASE_URL\" > " + quoteFixtureShell(marker) + "; cat"
			fixtureGit(t, view.WorkspaceRoot, "config", "filter.audit."+kind, filter)
			fixtureGit(t, view.WorkspaceRoot, "config", "filter.audit.required", "true")
			fixtureWrite(t, filepath.Join(view.RootPath, ".gitattributes"), []byte("tracked.txt filter=audit\n"))
			fixtureWrite(t, filepath.Join(view.RootPath, "tracked.txt"), []byte("safe raw content\n"))
			patch, err := coordinator.Worktrees.Diff(context.Background(), view)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(patch, []byte("+safe raw content")) {
				t.Fatalf("patch did not preserve edited content: %s", patch)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("Agent Git %s filter executed on the host: %v", kind, err)
			}
			if bytes.Contains(patch, []byte("fake-audit-secret")) {
				t.Fatal("host credential environment leaked into patch")
			}
		})
	}
}

func TestPatchCaptureDoesNotFollowEditedWorktreeGitPointer(t *testing.T) {
	coordinator, view := preservationFixture(t)
	outside := t.TempDir()
	marker := filepath.Join(outside, "pointer-filter-executed")
	fixtureGit(t, outside, "init", "-q")
	fixtureGit(t, outside, "config", "filter.audit.clean", "printf bad > "+quoteFixtureShell(marker)+"; cat")
	fixtureGit(t, outside, "config", "filter.audit.required", "true")
	fixtureWrite(t, filepath.Join(view.RootPath, ".git"), []byte("gitdir: "+filepath.Join(outside, ".git")+"\n"))
	fixtureWrite(t, filepath.Join(view.RootPath, ".gitattributes"), []byte("tracked.txt filter=audit\n"))
	fixtureWrite(t, filepath.Join(view.RootPath, "tracked.txt"), []byte("still belongs to original baseline\n"))
	patch, err := coordinator.Worktrees.Diff(context.Background(), view)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(patch, []byte("+still belongs to original baseline")) {
		t.Fatalf("patch lost original baseline binding: %s", patch)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("edited .git pointer executed external configuration: %v", err)
	}
}

func TestPatchCapturePreservesRepositoryLocalExclusions(t *testing.T) {
	coordinator, view := preservationFixture(t)
	fixtureWrite(t, filepath.Join(view.WorkspaceRoot, ".git", "info", "exclude"), []byte("private-local.txt\n"))
	fixtureWrite(t, filepath.Join(view.RootPath, "private-local.txt"), []byte("must stay ignored\n"))
	fixtureWrite(t, filepath.Join(view.RootPath, "new.txt"), []byte("must be published\n"))
	patch, err := coordinator.Worktrees.Diff(context.Background(), view)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(patch, []byte("private-local.txt")) || !bytes.Contains(patch, []byte("new.txt")) {
		t.Fatalf("repository-local exclude handling changed: %s", patch)
	}
}

func TestPatchCapturePreservesLocalLineEndingConversion(t *testing.T) {
	coordinator, view := preservationFixture(t)
	fixtureGit(t, view.WorkspaceRoot, "config", "core.autocrlf", "true")
	fixtureWrite(t, filepath.Join(view.RootPath, "tracked.txt"), []byte("base\r\n"))
	patch, err := coordinator.Worktrees.Diff(context.Background(), view)
	if err != nil {
		t.Fatal(err)
	}
	if len(patch) != 0 {
		t.Fatalf("line ending normalization became a content edit: %s", patch)
	}
}

func quoteFixtureShell(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

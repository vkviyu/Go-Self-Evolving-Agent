package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinkFilePathsKeepsCompleteTargetAcrossVisibleLabel(t *testing.T) {
	root := t.TempDir()
	relative := filepath.Join("internal", "app", "agent_factory.go")
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("package app\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	rendered := linkFilePaths("read "+filepath.ToSlash(relative)+":42", root)
	if !strings.Contains(rendered, "\x1b]8;;file://") {
		t.Fatalf("missing OSC 8 link: %q", rendered)
	}
	if !strings.Contains(rendered, "internal/app/agent_factory.go:42") {
		t.Fatalf("visible path label changed: %q", rendered)
	}
	if !strings.Contains(rendered, "#L42") {
		t.Fatalf("line anchor missing from target: %q", rendered)
	}
}

func TestLinkFilePathsLeavesUnknownTokensUntouched(t *testing.T) {
	const content = "internal/missing/file.go and ordinary/path/name"
	if got := linkFilePaths(content, t.TempDir()); got != content {
		t.Fatalf("unknown paths were linked: %q", got)
	}
}

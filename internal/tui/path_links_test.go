package tui

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/cloudwego/eino/schema"
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

func TestRestoredMarkdownFileLinksStayOnTheirOwnRows(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const relative = "docs/ARCHITECTURE.md"
	path := filepath.Join(workspace, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("# Architecture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	history := []*schema.Message{
		schema.UserMessage("Inspect the architecture."),
		schema.AssistantMessage(fmt.Sprintf("See [Recovery](%s:244), [Checkpoints](%s#L258).\n\nLocal reference `%s:310`.\n\nUnlinked paragraph.", filepath.ToSlash(path), filepath.ToSlash(path), relative), nil),
		schema.UserMessage("Continue the investigation."),
	}
	model, err := NewModel(context.Background(), Config{Conversation: &fakeConversation{id: "restored", history: history}, Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	model.blocks = append(model.blocks, transcriptBlock{kind: blockFailure, content: "execution paused (transport)"})
	wide := max(200, len(path)+80)
	for _, width := range []int{70, wide} {
		model.resize(width, 10)
		plain := ansi.Strip(model.transcriptContent)
		for _, want := range []string{"See Recovery", "Checkpoints", "Unlinked paragraph.", "Continue the investigation.", "execution paused (transport)"} {
			if !strings.Contains(plain, want) {
				t.Fatalf("width %d: visible text lost %q: %q", width, want, plain)
			}
		}
		for _, unwanted := range []string{"]8;;", "[Recovery](", "[Checkpoints](", "file://"} {
			if strings.Contains(plain, unwanted) {
				t.Fatalf("width %d: corrupt visible text %q: %q", width, unwanted, plain)
			}
		}
		anchors := []string{"L310"}
		if width == wide {
			anchors = append(anchors, "L244", "L258")
		}
		for _, anchor := range anchors {
			target := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path), Fragment: anchor}).String()
			if !strings.Contains(model.transcriptContent, target) {
				t.Fatalf("width %d: missing target %s in %q", width, target, plain)
			}
		}
		assertFileLinksClosedOnEveryRow(t, model.transcriptContent)
		// The viewport can start or end in the middle of the wrapped citation.
		for row := range strings.Count(model.transcriptContent, "\n") + 1 {
			model.viewport.SetYOffset(row)
			assertFileLinksClosedOnEveryRow(t, model.View())
		}
	}
}

func TestWrappedToolFileLinkRetainsCompleteTarget(t *testing.T) {
	model := newTestModel(t)
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	model.workspace = workspace
	relative := "docs/" + strings.Repeat("architecture-", 10) + "notes.md"
	path := filepath.Join(model.workspace, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	model.blocks = []transcriptBlock{{
		kind: blockTool, toolName: "workspace_read", argumentsExpanded: true,
		toolArguments: fmt.Sprintf(`{"path":%q}`, relative),
	}, {kind: blockUser, content: "Continue."}}
	model.resize(58, 9)
	target := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
	if count := strings.Count(model.transcriptContent, target); count < 2 {
		t.Fatalf("expected the complete link target on each wrapped row, found %d", count)
	}
	assertFileLinksClosedOnEveryRow(t, model.transcriptContent)
	for row := range strings.Count(model.transcriptContent, "\n") + 1 {
		model.viewport.SetYOffset(row)
		assertFileLinksClosedOnEveryRow(t, model.View())
	}
}

func assertFileLinksClosedOnEveryRow(t *testing.T, content string) {
	t.Helper()
	for row, line := range strings.Split(content, "\n") {
		opens := strings.Count(line, fileLinkPrefix+"file://")
		closes := strings.Count(line, ansi.ResetHyperlink())
		if opens != closes || strings.Contains(ansi.Strip(line), "]8;;") {
			t.Fatalf("row %d leaks a hyperlink: %q", row, line)
		}
	}
}

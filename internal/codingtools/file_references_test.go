package codingtools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestInspectFileReferencesFindsMetadataWithoutIncludingBody(t *testing.T) {
	root := t.TempDir()
	markdown := "# 架构\r\nprivate body\r\n```text\n# Ignored\n```\n## API ###\n末行"
	files := map[string]string{
		"docs/design.md":      markdown,
		"docs/with spaces.md": "# Introduction\n",
		"Makefile":            "all:\n",
		"docs/api.go":         "package api\n",
	}
	for path, content := range files {
		writeReferenceFixture(t, root, path, content)
	}
	workspace, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	query := "先读@docs/design.md，再查 `docs/design.md:5`、@\"" + filepath.Join(workspace.Root(), "docs/with spaces.md") + "\"、@Makefile 和 `docs/api.go:5`"
	got, err := workspace.InspectFileReferences(context.Background(), query, nil)
	if err != nil || len(got) != 4 {
		t.Fatalf("metadata = %#v, error = %v", got, err)
	}
	if got[0].Path != "docs/design.md" || got[1].Path != "docs/with spaces.md" || got[2].Path != "Makefile" || got[3].Path != "docs/api.go" {
		t.Fatalf("incorrect paths/order: %#v", got)
	}
	doc := got[0]
	if doc.SizeBytes != int64(len(markdown)) || doc.CharacterCount == nil || *doc.CharacterCount != utf8.RuneCountInString(markdown) || doc.LineCount == nil || *doc.LineCount != 7 {
		t.Fatalf("incorrect counts: %#v", doc)
	}
	wantHeadings := []FileHeading{{Line: 1, Level: 1, Title: "架构"}, {Line: 6, Level: 2, Title: "API"}}
	if !reflect.DeepEqual(doc.Headings, wantHeadings) || doc.HeadingsTruncated {
		t.Fatalf("headings = %#v, truncated = %v", doc.Headings, doc.HeadingsTruncated)
	}
}

func TestInspectFileReferencesFiltersUnsafeAndDeniedPaths(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("# Outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.md")); err != nil {
		t.Fatal(err)
	}
	writeReferenceFixture(t, root, "allowed.md", "# Visible\n")
	writeReferenceFixture(t, root, "denied.md", "# Denied\n")
	writeReferenceFixture(t, root, "docs/path.txt", "file content")
	workspace, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	var checked []string
	query := "@link.md @../outside.md @" + outside + " @" + filepath.Join(workspace.Root(), "denied.md") + " @docs docs/path.txt (allowed.md). @missing.md https://domain/allowed.md"
	got, err := workspace.InspectFileReferences(context.Background(), query, func(path string) bool {
		checked = append(checked, path)
		return path != "denied.md"
	})
	if err != nil || len(got) != 2 || got[0].Path != "docs/path.txt" || got[1].Path != "allowed.md" {
		t.Fatalf("metadata = %#v, error = %v", got, err)
	}
	wantChecks := []string{"link.md", "denied.md", "docs", "missing.md", "docs/path.txt", "allowed.md"}
	if !reflect.DeepEqual(checked, wantChecks) {
		t.Fatalf("checked paths = %#v", checked)
	}
}

func TestInspectFileReferencesBoundsLargeFileAndOutline(t *testing.T) {
	root := t.TempDir()
	prefix := "# Large document\n" + strings.Repeat("\n", maxReferenceFileScan)
	writeReferenceFixture(t, root, "large.md", prefix+"# Beyond scan\n")
	writeReferenceFixture(t, root, "binary.bin", "prefix\x00binary")
	var outline strings.Builder
	for i := range maxReferenceHeadings + 2 {
		fmt.Fprintf(&outline, "## Heading %d\n", i)
	}
	writeReferenceFixture(t, root, "outline.md", outline.String())
	workspace, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := workspace.InspectFileReferences(context.Background(), "large.md binary.bin outline.md", nil)
	if err != nil || len(got) != 3 {
		t.Fatalf("metadata = %#v, error = %v", got, err)
	}
	if got[0].SizeBytes != int64(len(prefix)+len("# Beyond scan\n")) || got[0].LineCount != nil || got[0].CharacterCount != nil || !got[0].HeadingsTruncated || len(got[0].Headings) != 1 {
		t.Fatalf("large document = %#v", got[0])
	}
	if got[1].LineCount != nil || got[1].CharacterCount != nil || len(got[1].Headings) != 0 {
		t.Fatalf("binary document = %#v", got[1])
	}
	if len(got[2].Headings) != maxReferenceHeadings || !got[2].HeadingsTruncated || got[2].LineCount == nil || *got[2].LineCount != maxReferenceHeadings+2 {
		t.Fatalf("outline = %#v", got[2])
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := workspace.InspectFileReferences(ctx, "large.md", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read: %v", err)
	}
}

func writeReferenceFixture(t *testing.T, root, path, content string) {
	t.Helper()
	absolute := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absolute, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

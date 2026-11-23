package codingtools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceReadLimitsSelectedRangeRatherThanFilePrefix(t *testing.T) {
	content := strings.Repeat("prefix", 100) + "\nsecond\nthird\nfourth\n"
	workspace := documentWorkspace(t, content)
	page := documentPage(t, workspace, `{"path":"doc.md","start_line":2,"end_line":3,"max_bytes":14}`)
	if page.Content != "second\nthird\n" || page.StartLine != 2 || page.EndLine != 3 || page.Truncated {
		t.Fatalf("incorrect selected range: %+v", page)
	}
	if !page.FingerprintComplete || page.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(content))) {
		t.Fatalf("incorrect full-file fingerprint: %+v", page)
	}
}

func TestWorkspaceReadContinuesBeyondFingerprintLimit(t *testing.T) {
	prefix := strings.Repeat("a", maximumReadBytes+1) + "\n"
	workspace := documentWorkspace(t, prefix+"alpha\nbeta\ngamma")
	page := documentPage(t, workspace, `{"path":"doc.md","start_line":2,"max_bytes":8}`)
	if page.Content != "alpha\n" || page.Offset != int64(len(prefix)) || page.EndLine != 2 || !page.Truncated || page.NextStartLine != 3 || page.NextOffset != int64(len(prefix)+len("alpha\n")) {
		t.Fatalf("incorrect continuation metadata: %+v", page)
	}
	if page.FingerprintComplete || page.SHA256 != "" {
		t.Fatalf("large file returned a full-file fingerprint: %+v", page)
	}
	linePage := documentPage(t, workspace, fmt.Sprintf(`{"path":"doc.md","start_line":%d}`, page.NextStartLine))
	if linePage.Content != "beta\ngamma" || linePage.EndLine != 4 || linePage.Truncated {
		t.Fatalf("line continuation lost content: %+v", linePage)
	}
	bytePage := documentPage(t, workspace, fmt.Sprintf(`{"path":"doc.md","offset":%d}`, page.NextOffset))
	if bytePage.Content != linePage.Content || bytePage.Truncated {
		t.Fatalf("byte continuation lost content: %+v", bytePage)
	}
}

func TestWorkspaceReadContinuesAnOversizedLineAtAUTF8Boundary(t *testing.T) {
	workspace := documentWorkspace(t, "中😀末\n")
	first := documentPage(t, workspace, `{"path":"doc.md","max_bytes":5}`)
	if first.Content != "中" || first.NextOffset != int64(len("中")) || first.NextStartLine != 0 || !first.Truncated {
		t.Fatalf("incorrect first fragment: %+v", first)
	}
	second := documentPage(t, workspace, fmt.Sprintf(`{"path":"doc.md","offset":%d}`, first.NextOffset))
	if second.Content != "😀末\n" || second.Truncated {
		t.Fatalf("UTF-8 continuation lost content: %+v", second)
	}
}

func documentWorkspace(t *testing.T, content string) *Workspace {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "doc.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return workspace
}

func documentPage(t *testing.T, workspace *Workspace, arguments string) readResult {
	t.Helper()
	output, err := workspace.read(context.Background(), json.RawMessage(arguments))
	if err != nil {
		t.Fatal(err)
	}
	var page readResult
	if err := json.Unmarshal(output, &page); err != nil {
		t.Fatal(err)
	}
	return page
}

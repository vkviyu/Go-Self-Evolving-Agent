package codingtools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteNeverReadsExternalLeafLink(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("private host content"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	workspace, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	result, err := workspace.write(context.Background(), json.RawMessage(`{"path":"link","content":"replacement"}`))
	if err == nil || strings.Contains(string(result), "private host content") {
		t.Fatalf("external link accepted: result=%s err=%v", result, err)
	}
	data, err := os.ReadFile(secret)
	if err != nil || string(data) != "private host content" {
		t.Fatalf("host content changed: %q %v", data, err)
	}
}

func TestStructuredWriteValidatesModeWithoutSchemaAdapter(t *testing.T) {
	workspace, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"path":"x","content":"x","mode":-1}`, `{"path":"x","content":"x","mode":512}`} {
		if _, err := workspace.write(context.Background(), json.RawMessage(raw)); err == nil {
			t.Fatalf("invalid mode accepted: %s", raw)
		}
	}
}

func TestLargeReadHasNoMisleadingFullFingerprint(t *testing.T) {
	root := t.TempDir()
	file, err := os.Create(filepath.Join(root, "large"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maximumReadBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	workspace, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	result, err := workspace.read(context.Background(), json.RawMessage(`{"path":"large","max_bytes":8}`))
	if err != nil {
		t.Fatal(err)
	}
	var value struct {
		SHA256    string `json:"sha256"`
		Complete  bool   `json:"fingerprint_complete"`
		Truncated bool   `json:"truncated"`
		Content   string `json:"content"`
	}
	if err := json.Unmarshal(result, &value); err != nil {
		t.Fatal(err)
	}
	if value.SHA256 != "" || value.Complete || !value.Truncated || len(value.Content) != 8 {
		t.Fatalf("unexpected read: %+v", value)
	}
	if _, err := workspace.edit(context.Background(), json.RawMessage(`{"path":"large","old_text":"x","new_text":"y"}`)); err == nil {
		t.Fatal("oversize edit accepted")
	}
}

func TestEditRejectsExpandedOutputBeforeAllocating(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "expand"), []byte(strings.Repeat("x", 4096)), 0600); err != nil {
		t.Fatal(err)
	}
	workspace, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := json.Marshal(map[string]any{"path": "expand", "old_text": "x", "new_text": strings.Repeat("y", 4096), "replace_all": true})
	if _, err := workspace.edit(context.Background(), request); err == nil {
		t.Fatal("oversize expanded edit accepted")
	}
}

package artifact

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStorePutReadAndDeduplicates(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("patch contents")
	first, err := store.Put(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Put(context.Background(), data)
	if err != nil || first != second {
		t.Fatalf("deduplicated references = %q, %q err=%v", first, second, err)
	}
	read, err := store.ReadArtifact(context.Background(), first)
	if err != nil || string(read) != string(data) {
		t.Fatalf("read artifact = %q err=%v", read, err)
	}
}

func TestStoreRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	store := &Store{Root: root}
	if _, err := store.ReadArtifact(context.Background(), "linked/secret"); err == nil {
		t.Fatal("symlink escape was accepted")
	} else if errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected context error: %v", err)
	}
}

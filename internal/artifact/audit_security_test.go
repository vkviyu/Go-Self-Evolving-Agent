package artifact

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContentAddressedReadRejectsTamperedObject(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reference, err := store.Put(context.Background(), []byte("trusted"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Root, "sha256", strings.TrimPrefix(reference, "sha256:"))
	if err := os.WriteFile(path, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadArtifact(context.Background(), reference); err == nil {
		t.Fatal("corrupt content accepted")
	}
}

func TestReadSegmentVerifiesAndRetainsOnlyRequestedRange(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := strings.Repeat("0123456789", 10000)
	reference, err := store.Put(context.Background(), []byte(data))
	if err != nil {
		t.Fatal(err)
	}
	segment, total, err := store.ReadSegment(context.Background(), reference, 32765, 8)
	if err != nil || total != int64(len(data)) || string(segment) != data[32765:32773] {
		t.Fatalf("segment=%q total=%d err=%v", segment, total, err)
	}
	if _, err := store.ReadArtifact(context.Background(), "arbitrary/file"); err == nil {
		t.Fatal("non-addressed reference accepted")
	}
}

package codingtools

import (
	"context"
	"strings"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/lsp"
)

type memoryDiffArtifacts struct {
	data []byte
}

func (store *memoryDiffArtifacts) Put(_ context.Context, data []byte) (string, error) {
	store.data = append([]byte(nil), data...)
	return "sha256:test-diff", nil
}

func TestBuildDiffExternalizesLargeUnifiedDiff(t *testing.T) {
	old := strings.Repeat("old line\n", 20_000)
	new := strings.Repeat("new line\n", 20_000)
	artifacts := &memoryDiffArtifacts{}
	result := buildDiff(context.Background(), artifacts, "large.txt", lsp.ChangeModified, []byte(old), []byte(new))
	if result.Status != "partial" || !result.Truncated {
		t.Fatalf("result = %#v, want partial truncated diff", result)
	}
	if result.ArtifactRef != "sha256:test-diff" || len(artifacts.data) == 0 {
		t.Fatalf("artifact = %#v bytes=%d", result, len(artifacts.data))
	}
	if result.Preview == "" || result.Bytes <= maxInlineDiffBytes {
		t.Fatalf("result preview/size = %q/%d", result.Preview, result.Bytes)
	}
}

func TestBuildDiffMarksBinaryContent(t *testing.T) {
	result := buildDiff(context.Background(), nil, "data.bin", lsp.ChangeModified, []byte("before\x00"), []byte("after\x00"))
	if result.Status != "binary" || result.Content != "" {
		t.Fatalf("binary result = %#v", result)
	}
}

func TestBuildDiffPreservesEmptyCreateAndDelete(t *testing.T) {
	created := buildDiff(context.Background(), nil, "empty.txt", lsp.ChangeCreated, nil, nil)
	if created.Status != "ready" || created.Message == "" {
		t.Fatalf("empty create = %#v", created)
	}
	deleted := buildDiff(context.Background(), nil, "empty.txt", lsp.ChangeDeleted, nil, nil)
	if deleted.Status != "ready" || deleted.Message == "" {
		t.Fatalf("empty delete = %#v", deleted)
	}
}

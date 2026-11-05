package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/artifact"
)

func TestArtifactReadBindingReturnsBoundedSegments(t *testing.T) {
	t.Parallel()
	store, err := artifact.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content := strings.Repeat("0123456789", 20)
	reference, err := store.Put(context.Background(), []byte(content))
	if err != nil {
		t.Fatal(err)
	}
	result, err := artifactReadBinding(store).Executor.Invoke(context.Background(), json.RawMessage(`{"reference":"`+reference+`","offset":10,"max_bytes":20}`))
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Content    string `json:"content"`
		NextOffset int    `json:"next_offset"`
		Truncated  bool   `json:"truncated"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		t.Fatal(err)
	}
	if response.Content != content[10:30] || response.NextOffset != 30 || !response.Truncated {
		t.Fatalf("artifact segment = %#v", response)
	}
}

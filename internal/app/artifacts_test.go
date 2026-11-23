package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
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

func TestArtifactReadPagesFitModelContextAndReconstructArtifact(t *testing.T) {
	for _, test := range []struct {
		name    string
		content string
	}{
		{"escaped text", strings.Repeat("{\"section\":\"中文<&>\"}\n", 3500)},
		{"binary", strings.Repeat("\xff\x00\xc2", 22000)},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := artifact.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			reference, err := store.Put(ctx, []byte(test.content))
			if err != nil {
				t.Fatal(err)
			}
			reader := artifactReadBinding(store).Executor
			var combined strings.Builder
			for offset := 0; ; {
				args, err := json.Marshal(map[string]any{"reference": reference, "offset": offset, "max_bytes": len(test.content)})
				if err != nil {
					t.Fatal(err)
				}
				result, err := reader.Invoke(ctx, args)
				if err != nil {
					t.Fatal(err)
				}
				if len(result) > agentruntime.InlineToolResultBytes {
					t.Fatalf("artifact page would be externalized again: %d bytes", len(result))
				}
				var page struct {
					Content    string `json:"content"`
					Base64     string `json:"base64"`
					Bytes      int    `json:"bytes"`
					NextOffset int    `json:"next_offset"`
					Truncated  bool   `json:"truncated"`
				}
				if err := json.Unmarshal(result, &page); err != nil {
					t.Fatal(err)
				}
				if page.Base64 != "" {
					data, err := base64.StdEncoding.DecodeString(page.Base64)
					if err != nil {
						t.Fatal(err)
					}
					page.Content = string(data)
				}
				if page.Bytes == 0 || page.Bytes != len(page.Content) {
					t.Fatalf("page returned inconsistent byte count: %d", page.Bytes)
				}
				combined.WriteString(page.Content)
				if !page.Truncated {
					break
				}
				if page.NextOffset != offset+page.Bytes || page.NextOffset >= len(test.content) {
					t.Fatalf("next offset = %d for offset %d and length %d", page.NextOffset, offset, page.Bytes)
				}
				offset = page.NextOffset
			}
			if combined.String() != test.content {
				t.Fatalf("combined pages differ from original artifact (%d vs %d bytes)", combined.Len(), len(test.content))
			}
		})
	}
}

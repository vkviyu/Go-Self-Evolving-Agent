package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/vkviyu/go-self-evolving-agent/internal/artifact"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

const maxArtifactReadBytes = 1 << 20

func artifactReadBinding(store *artifact.Store) tool.Binding {
	return tool.Binding{
		Descriptor: tool.Descriptor{
			Name: "artifact_read", Description: "Read a bounded segment of a content-addressed artifact referenced by context management or orchestration.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["reference"],"properties":{"reference":{"type":"string","minLength":1},"offset":{"type":"integer","minimum":0},"max_bytes":{"type":"integer","minimum":1,"maximum":1048576}}}`),
			Revision:    "builtin-v1", RiskClass: tool.RiskRead, Capabilities: []string{"artifact.read"},
		},
		Executor: tool.InvokeFunc(func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			var request struct {
				Reference string `json:"reference"`
				Offset    int    `json:"offset"`
				MaxBytes  int    `json:"max_bytes"`
			}
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&request); err != nil {
				return nil, fmt.Errorf("decode artifact_read request: %w", err)
			}
			if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
				return nil, errors.New("artifact_read request contains trailing data")
			}
			if strings.TrimSpace(request.Reference) == "" || request.Offset < 0 || request.MaxBytes < 0 || request.MaxBytes > maxArtifactReadBytes {
				return nil, errors.New("artifact_read requires a reference and bounded non-negative offset/max_bytes")
			}
			limit := request.MaxBytes
			if limit == 0 {
				limit = 64 << 10
			}
			segment, total, err := store.ReadSegment(ctx, request.Reference, int64(request.Offset), int64(limit))
			if err != nil {
				return nil, err
			}
			end := request.Offset + len(segment)
			response := map[string]any{
				"reference": request.Reference, "offset": request.Offset, "bytes": len(segment),
				"total_bytes": total, "truncated": int64(end) < total,
			}
			if int64(end) < total {
				response["next_offset"] = end
			}
			if utf8.Valid(segment) {
				response["content"] = string(segment)
			} else {
				response["base64"] = base64.StdEncoding.EncodeToString(segment)
				response["encoding"] = "base64"
			}
			return json.Marshal(response)
		}),
	}
}

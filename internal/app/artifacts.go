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

	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/artifact"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

const (
	maxArtifactReadBytes = 1 << 20
	artifactPageOverhead = 512 // room for the reference, byte counts and JSON keys
)

func artifactReadBinding(store *artifact.Store) tool.Binding {
	return tool.Binding{
		Descriptor: tool.Descriptor{
			Name: "artifact_read", Description: "Read a model-sized page of a content-addressed artifact referenced by context management or orchestration. Follow next_offset to retrieve additional pages. max_bytes is an upper bound; the tool may return a shorter page to keep it readable in model context.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["reference"],"properties":{"reference":{"type":"string","minLength":1},"offset":{"type":"integer","minimum":0},"max_bytes":{"type":"integer","minimum":1,"maximum":1048576}}}`),
			Revision:    "builtin-v2", RiskClass: tool.RiskRead, Capabilities: []string{"artifact.read"},
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
			limit := agentruntime.InlineToolResultBytes - artifactPageOverhead
			if request.MaxBytes > 0 {
				limit = min(request.MaxBytes, limit)
			}
			segment, total, err := store.ReadSegment(ctx, request.Reference, int64(request.Offset), int64(limit))
			if err != nil {
				return nil, err
			}
			return encodeArtifactPage(request.Reference, request.Offset, segment, total)
		}),
	}
}

func encodeArtifactPage(reference string, offset int, segment []byte, total int64) (json.RawMessage, error) {
	for {
		if int64(offset)+int64(len(segment)) < total {
			segment = trimArtifactPartialRune(segment)
		}
		end := int64(offset) + int64(len(segment))
		response := map[string]any{
			"reference": reference, "offset": offset, "bytes": len(segment),
			"total_bytes": total, "truncated": end < total,
		}
		if end < total {
			response["next_offset"] = end
		}
		if utf8.Valid(segment) {
			response["content"] = string(segment)
		} else {
			response["base64"] = base64.StdEncoding.EncodeToString(segment)
			response["encoding"] = "base64"
		}
		encoded, err := json.Marshal(response)
		if err != nil || len(encoded) <= agentruntime.InlineToolResultBytes {
			return encoded, err
		}
		// Escaped text or Base64 can exceed the encoded budget. Keep a
		// shorter prefix and report its exact continuation offset.
		segment = segment[:len(segment)/2]
	}
}

func trimArtifactPartialRune(segment []byte) []byte {
	// Leave binary data and tiny explicitly requested pages usable via Base64.
	for start := len(segment) - 1; start > 0 && len(segment)-start < utf8.UTFMax; start-- {
		if !utf8.RuneStart(segment[start]) {
			continue
		}
		if !utf8.FullRune(segment[start:]) && utf8.Valid(segment[:start]) {
			return segment[:start]
		}
		break
	}
	return segment
}

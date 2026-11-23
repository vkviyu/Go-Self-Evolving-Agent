package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cloudwego/eino/schema"
)

const (
	// InlineToolResultBytes is the encoded result size that can enter model
	// context directly. The artifact reader uses the same bound for its pages.
	InlineToolResultBytes  = 24 << 10
	contextArtifactPreview = 2 << 10
)

type contextArtifact struct {
	Reference string `json:"reference"`
	Bytes     int    `json:"bytes"`
	Message   int    `json:"message_index"`
}

func (f *Factory) externalizeToolResults(ctx context.Context, messages []*schema.Message) ([]*schema.Message, []contextArtifact, error) {
	if f.artifacts == nil {
		return messages, nil, nil
	}
	prepared := make([]*schema.Message, len(messages))
	copy(prepared, messages)
	artifacts := make([]contextArtifact, 0)
	for index, message := range messages {
		if message == nil || message.Role != schema.Tool || len(message.Content) <= InlineToolResultBytes {
			continue
		}
		reference, err := f.artifacts.Put(ctx, []byte(message.Content))
		if err != nil {
			return nil, nil, fmt.Errorf("externalize tool result at message %d: %w", index, err)
		}
		headEnd := utf8PrefixBoundary(message.Content, min(contextArtifactPreview, len(message.Content)))
		tailStart := utf8SuffixBoundary(message.Content, max(0, len(message.Content)-contextArtifactPreview))
		placeholder, err := json.Marshal(map[string]any{
			"artifact_ref":  reference,
			"content_bytes": len(message.Content),
			"preview_head":  message.Content[:headEnd],
			"preview_tail":  message.Content[tailStart:],
			"retrieval":     "Use artifact_read with this reference. Follow next_offset for additional pages.",
		})
		if err != nil {
			return nil, nil, err
		}
		clone := *message
		clone.Content = string(placeholder)
		prepared[index] = &clone
		artifacts = append(artifacts, contextArtifact{Reference: reference, Bytes: len(message.Content), Message: index})
	}
	return prepared, artifacts, nil
}

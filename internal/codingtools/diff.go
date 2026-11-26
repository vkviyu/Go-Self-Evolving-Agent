package codingtools

import (
	"bytes"
	"context"
	"unicode/utf8"

	"github.com/aymanbagabas/go-udiff"
)

const (
	maxDiffInputBytes       = 4 << 20
	maxInlineDiffBytes      = 96 << 10
	maxInlineDiffTotalBytes = 1 << 20
	maxReportedDiffs        = 128
)

// DiffArtifactStore is the narrow content-addressed storage capability used
// when a unified diff is too large to keep inline in a tool result.
type DiffArtifactStore interface {
	Put(context.Context, []byte) (string, error)
}

type diffResult struct {
	Path        string     `json:"path"`
	Kind        changeKind `json:"kind"`
	Status      string     `json:"status"`
	Bytes       int        `json:"bytes,omitempty"`
	Content     string     `json:"content,omitempty"`
	ArtifactRef string     `json:"artifact_ref,omitempty"`
	Preview     string     `json:"preview,omitempty"`
	Truncated   bool       `json:"truncated,omitempty"`
	Message     string     `json:"message,omitempty"`
}

func buildDiff(ctx context.Context, store DiffArtifactStore, relative string, kind changeKind, before, after []byte) diffResult {
	result := diffResult{Path: relative, Kind: kind, Status: "ready"}
	if kind == changeModified && bytes.Equal(before, after) {
		result.Status = "unchanged"
		return result
	}
	if kind == changeCreated && len(after) == 0 {
		result.Message = "empty file created"
		return result
	}
	if kind == changeDeleted && len(before) == 0 {
		result.Message = "empty file deleted"
		return result
	}
	if len(before) > maxDiffInputBytes || len(after) > maxDiffInputBytes {
		result.Status = "unavailable"
		result.Truncated = true
		result.Message = "one or both file versions exceed the unified diff input limit"
		return result
	}
	if isBinary(before) || isBinary(after) {
		result.Status = "binary"
		result.Message = "binary files are changed, but textual unified diff is unavailable"
		return result
	}
	oldLabel, newLabel := "a/"+relative, "b/"+relative
	if kind == changeCreated {
		oldLabel = "/dev/null"
	}
	if kind == changeDeleted {
		newLabel = "/dev/null"
	}
	content := udiff.Unified(oldLabel, newLabel, string(before), string(after))
	if content == "" {
		result.Status = "unchanged"
		return result
	}
	result.Bytes = len(content)
	if len(content) <= maxInlineDiffBytes {
		result.Content = content
		return result
	}
	spillDiff(ctx, store, &result, content, "unified diff exceeds the inline limit and artifact storage is unavailable")
	return result
}

// spillDiff replaces inline diff content with a bounded preview and moves the
// full diff into artifact storage when it is available. unstoredMessage
// explains the truncation when there is no store.
func spillDiff(ctx context.Context, store DiffArtifactStore, result *diffResult, content, unstoredMessage string) {
	result.Content = ""
	result.Status = "partial"
	result.Truncated = true
	result.Preview = diffPreview(content)
	if store == nil {
		result.Message = unstoredMessage
		return
	}
	reference, err := store.Put(ctx, []byte(content))
	if err != nil {
		result.Message = "unified diff artifact could not be persisted: " + err.Error()
		return
	}
	result.ArtifactRef = reference
	result.Message = "full unified diff is available from artifact_ref"
}

func isBinary(data []byte) bool {
	return bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data)
}

func diffPreview(content string) string {
	limit := maxInlineDiffBytes / 2
	if len(content) <= limit {
		return content
	}
	preview := content[:limit]
	for len(preview) > 0 && !utf8.ValidString(preview) {
		preview = preview[:len(preview)-1]
	}
	return preview + "\n... diff preview truncated ..."
}

// appendFileChange adds the change summary and single-file diff to a write result.
func appendFileChange(ctx context.Context, store DiffArtifactStore, result map[string]any, relative string, kind changeKind, before, after []byte) {
	result["changes"] = []changeSummary{{Path: relative, Kind: kind}}
	result["diff"] = buildDiff(ctx, store, relative, kind, before, after)
}

func appendDiffs(ctx context.Context, store DiffArtifactStore, result map[string]any, diffs []diffResult, alreadyTruncated bool) {
	if alreadyTruncated || len(diffs) > maxReportedDiffs {
		result["diffs_truncated"] = true
	}
	if len(diffs) == 0 {
		return
	}
	if len(diffs) > maxReportedDiffs {
		diffs = diffs[:maxReportedDiffs]
	}
	inlineBytes := 0
	for index := range diffs {
		if diffs[index].Content == "" {
			continue
		}
		if inlineBytes+len(diffs[index].Content) <= maxInlineDiffTotalBytes {
			inlineBytes += len(diffs[index].Content)
			continue
		}
		spillDiff(ctx, store, &diffs[index], diffs[index].Content, "unified diff exceeds the aggregate inline limit")
	}
	result["diffs"] = diffs
}

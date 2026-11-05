package workspacecoord

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	udiff "github.com/aymanbagabas/go-udiff"
)

const (
	// planDiffLimit bounds the diff shown for an approval or a review.
	planDiffLimit = 64 << 10
)

// PlanDiff renders the plan as a unified diff of main against the planned
// postimages. Binary files and symlinks are summarized in one line. Output
// beyond planDiffLimit is cut with a note.
func PlanDiff(ctx context.Context, root string, plan ApplyPlan, artifacts ArtifactReader) (string, error) {
	var builder strings.Builder
	for _, entry := range plan.Entries {
		if err := contextError(ctx); err != nil {
			return "", err
		}
		before, err := imageText(ctx, entry.Preimage, filepath.Join(root, filepath.FromSlash(entry.Path)), "", nil)
		if err != nil {
			return "", err
		}
		after, err := imageText(ctx, entry.Postimage, "", entry.Artifact, artifacts)
		if err != nil {
			return "", err
		}
		builder.WriteString(describeChange(entry.Path, before, after))
		if builder.Len() > planDiffLimit {
			text := builder.String()[:planDiffLimit]
			return text + fmt.Sprintf("\n... diff truncated at %d bytes\n", planDiffLimit), nil
		}
	}
	return builder.String(), nil
}

// CandidateDiff renders a candidate as a unified diff of its baseline
// against its work tree, for reviewers of a snapshot task.
func CandidateDiff(ctx context.Context, candidate Candidate, artifacts ArtifactReader) (string, error) {
	var builder strings.Builder
	for _, change := range candidate.Entries {
		if err := contextError(ctx); err != nil {
			return "", err
		}
		var base, next *ManifestEntry
		if change.Change != ChangeAdd {
			base = &change.Base
		}
		if change.Change != ChangeDelete {
			next = &change.New
		}
		before, err := imageText(ctx, base, "", change.BaseArtifact, artifacts)
		if err != nil {
			return "", err
		}
		after, err := imageText(ctx, next, "", change.Artifact, artifacts)
		if err != nil {
			return "", err
		}
		builder.WriteString(describeChange(change.Path, before, after))
		if builder.Len() > planDiffLimit {
			text := builder.String()[:planDiffLimit]
			return text + fmt.Sprintf("\n... diff truncated at %d bytes\n", planDiffLimit), nil
		}
	}
	return builder.String(), nil
}

// imageText loads the text of one image for a diff. It returns nil for an
// absent image, and a one-line summary for a symlink or binary file. File
// content comes from path when it is set, otherwise from the artifact.
func imageText(ctx context.Context, image *ManifestEntry, path, reference string, artifacts ArtifactReader) (*string, error) {
	if image == nil {
		return nil, nil
	}
	if image.Type == EntrySymlink {
		summary := "symlink -> " + image.Target + "\n"
		return &summary, nil
	}
	var content []byte
	var err error
	if path != "" {
		content, err = os.ReadFile(path)
	} else if artifacts == nil {
		err = errors.New("content artifact reader is unavailable")
	} else {
		content, err = artifacts.ReadArtifact(ctx, reference)
	}
	if err != nil {
		return nil, fmt.Errorf("read diff content of %q: %w", image.Path, err)
	}
	text := string(content)
	if !isText(text) {
		text = fmt.Sprintf("binary file, %d bytes, %s\n", len(content), image.SHA256)
	}
	return &text, nil
}

// describeChange formats one path change as a unified diff.
func describeChange(path string, before, after *string) string {
	oldLabel, newLabel := "a/"+path, "b/"+path
	var oldText, newText string
	if before == nil {
		oldLabel = "/dev/null"
	} else {
		oldText = *before
	}
	if after == nil {
		newLabel = "/dev/null"
	} else {
		newText = *after
	}
	diff := udiff.Unified(oldLabel, newLabel, oldText, newText)
	if diff == "" {
		// Only the mode changed.
		return fmt.Sprintf("--- %s\n+++ %s\n(mode changed)\n", oldLabel, newLabel)
	}
	return diff
}

package codingtools

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	maxTrackedWorkspaceFiles = 50_000
	maxReportedChanges       = 2_000
	maxDiffSnapshotBytes     = 8 << 20

	changeCreated  changeKind = "created"
	changeModified changeKind = "modified"
	changeDeleted  changeKind = "deleted"
)

type (
	changeKind string

	changeSummary struct {
		Path string     `json:"path"`
		Kind changeKind `json:"kind"`
	}

	fileStamp struct {
		size       int64
		modifiedNS int64
		mode       fs.FileMode
		content    []byte
		contentOK  bool
	}

	workspaceSnapshot struct {
		files     map[string]fileStamp
		truncated bool
		message   string
	}

	changeCapture struct {
		changes        []changeSummary
		truncated      bool
		message        string
		diffs          []diffResult
		diffsTruncated bool
	}
)

// snapshotWorkspace records file metadata and a bounded set of bytes for diffs.
func snapshotWorkspace(ctx context.Context, root string) (workspaceSnapshot, error) {
	files, err := os.OpenRoot(root)
	if err != nil {
		return workspaceSnapshot{}, err
	}
	defer files.Close()
	snapshot := workspaceSnapshot{files: make(map[string]fileStamp)}
	contentBytes := 0
	err = fs.WalkDir(files.FS(), ".", func(relative string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			snapshot.truncated = true
			snapshot.message = joinCaptureMessage(snapshot.message, walkErr.Error())
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if relative == ".git" || relative == ".gsea/artifacts" {
				return filepath.SkipDir
			}
			return nil
		}
		if len(snapshot.files) >= maxTrackedWorkspaceFiles {
			snapshot.truncated = true
			snapshot.message = joinCaptureMessage(snapshot.message, fmt.Sprintf("workspace change scan limited to %d files", maxTrackedWorkspaceFiles))
			return fs.SkipAll
		}
		info, err := entry.Info()
		if err != nil {
			snapshot.truncated = true
			snapshot.message = joinCaptureMessage(snapshot.message, err.Error())
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		stamp := fileStamp{size: info.Size(), modifiedNS: info.ModTime().UnixNano(), mode: info.Mode()}
		limit := min(maxDiffInputBytes, maxDiffSnapshotBytes-contentBytes)
		if info.Size() <= int64(limit) {
			data, truncated, readErr := readRootBounded(ctx, files, filepath.FromSlash(relative), limit)
			if readErr != nil {
				snapshot.truncated = true
				snapshot.message = joinCaptureMessage(snapshot.message, readErr.Error())
			} else if !truncated {
				stamp.content = data
				stamp.contentOK = true
				contentBytes += len(data)
			}
		}
		snapshot.files[relative] = stamp
		return nil
	})
	if err != nil {
		return workspaceSnapshot{}, err
	}
	return snapshot, nil
}

// captureWorkspaceChanges compares snapshots and produces bounded tool output.
func captureWorkspaceChanges(ctx context.Context, store DiffArtifactStore, before, after workspaceSnapshot) changeCapture {
	paths := make([]string, 0)
	seen := make(map[string]struct{}, len(before.files)+len(after.files))
	for path := range before.files {
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	for path := range after.files {
		if _, exists := seen[path]; !exists {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	capture := changeCapture{
		changes:   make([]changeSummary, 0),
		truncated: before.truncated || after.truncated,
		message:   joinCaptureMessage(before.message, after.message),
	}
	for _, relative := range paths {
		oldStamp, existed := before.files[relative]
		newStamp, exists := after.files[relative]
		kind := changeModified
		switch {
		case !existed && exists:
			kind = changeCreated
		case existed && !exists:
			kind = changeDeleted
		case sameStamp(oldStamp, newStamp):
			continue
		}
		if len(capture.changes) == maxReportedChanges {
			capture.truncated = true
			capture.message = joinCaptureMessage(capture.message, fmt.Sprintf("reported changes limited to %d files", maxReportedChanges))
			break
		}
		capture.changes = append(capture.changes, changeSummary{Path: relative, Kind: kind})
		switch {
		case len(capture.diffs) == maxReportedDiffs:
			capture.diffsTruncated = true
		case (!existed || oldStamp.contentOK) && (!exists || newStamp.contentOK):
			capture.diffs = append(capture.diffs, buildDiff(ctx, store, relative, kind, oldStamp.content, newStamp.content))
		default:
			capture.diffs = append(capture.diffs, diffResult{
				Path: relative, Kind: kind, Status: "unavailable", Truncated: true,
				Message: "one or both file versions were not captured within the diff limits",
			})
		}
	}
	return capture
}

// sameStamp compares captured content when available and otherwise uses metadata.
func sameStamp(left, right fileStamp) bool {
	return left.size == right.size && left.modifiedNS == right.modifiedNS && left.mode == right.mode &&
		(!left.contentOK || !right.contentOK || bytes.Equal(left.content, right.content))
}

func joinCaptureMessage(left, right string) string {
	left, right = strings.TrimSpace(left), strings.TrimSpace(right)
	if left == "" {
		return right
	}
	if right == "" || left == right {
		return left
	}
	return left + "; " + right
}

// snapshotAfterMutation retains truncation information if the root is unavailable.
func snapshotAfterMutation(ctx context.Context, root string) workspaceSnapshot {
	snapshot, err := snapshotWorkspace(ctx, root)
	if err != nil {
		return workspaceSnapshot{truncated: true, message: err.Error()}
	}
	return snapshot
}

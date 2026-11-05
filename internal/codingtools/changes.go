package codingtools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/vkviyu/go-self-evolving-agent/internal/lsp"
)

const (
	maxTrackedWorkspaceFiles = 50_000
	maxReportedChanges       = 2_000
	maxDiagnosedChanges      = 128
	maxDiagnosticSourceBytes = 4 << 20
	maxDiffSnapshotBytes     = 8 << 20
)

type fileStamp struct {
	size       int64
	modifiedNS int64
	mode       fs.FileMode
	revision   string
	content    []byte
	contentOK  bool
}

type workspaceSnapshot struct {
	files        map[string]fileStamp
	contentBytes int64
	truncated    bool
	message      string
}

type changeCapture struct {
	set            lsp.ChangeSet
	reported       []lsp.ChangeSummary
	diffs          []diffResult
	diffsTruncated bool
}

func sourceChange(path, relative string, kind lsp.ChangeKind, content []byte) lsp.Change {
	digest := sha256.Sum256(content)
	return lsp.Change{
		Path: path, Relative: relative, Kind: kind,
		Revision: hex.EncodeToString(digest[:]), Content: append([]byte(nil), content...),
	}
}

func snapshotWorkspace(ctx context.Context, root string) (workspaceSnapshot, error) {
	snapshot := workspaceSnapshot{files: make(map[string]fileStamp)}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
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
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
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
		if info.Size() <= maxDiffInputBytes && snapshot.contentBytes+info.Size() <= maxDiffSnapshotBytes {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				snapshot.truncated = true
				snapshot.message = joinCaptureMessage(snapshot.message, readErr.Error())
			} else {
				stamp.content = data
				stamp.contentOK = true
				snapshot.contentBytes += info.Size()
				if lsp.SupportsPath(path) {
					digest := sha256.Sum256(data)
					stamp.revision = hex.EncodeToString(digest[:])
				}
			}
		} else if lsp.SupportsPath(path) && info.Size() <= maxDiagnosticSourceBytes {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				snapshot.truncated = true
				snapshot.message = joinCaptureMessage(snapshot.message, readErr.Error())
			} else {
				digest := sha256.Sum256(data)
				stamp.revision = hex.EncodeToString(digest[:])
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

func captureWorkspaceChanges(ctx context.Context, store DiffArtifactStore, root string, before, after workspaceSnapshot) changeCapture {
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
		set:      lsp.ChangeSet{Root: root, Truncated: before.truncated || after.truncated, Message: joinCaptureMessage(before.message, after.message)},
		reported: make([]lsp.ChangeSummary, 0),
		diffs:    make([]diffResult, 0),
	}
	for _, relative := range paths {
		oldStamp, existed := before.files[relative]
		newStamp, exists := after.files[relative]
		kind := lsp.ChangeModified
		switch {
		case !existed && exists:
			kind = lsp.ChangeCreated
		case existed && !exists:
			kind = lsp.ChangeDeleted
		case sameStamp(oldStamp, newStamp):
			continue
		}
		change := lsp.Change{Path: filepath.Join(root, filepath.FromSlash(relative)), Relative: relative, Kind: kind}
		if exists {
			change.Revision = newStamp.revision
		}
		capture.reported = append(capture.reported, lsp.ChangeSummary{Path: relative, Kind: kind, Revision: change.Revision})
		var oldContent, newContent []byte
		contentAvailable := true
		if existed {
			if oldStamp.contentOK {
				oldContent = oldStamp.content
			} else {
				contentAvailable = false
			}
		}
		if exists {
			if newStamp.contentOK {
				newContent = newStamp.content
			} else {
				contentAvailable = false
			}
		}
		if len(capture.diffs) >= maxReportedDiffs {
			capture.diffsTruncated = true
		} else if contentAvailable {
			capture.diffs = append(capture.diffs, buildDiff(ctx, store, relative, kind, oldContent, newContent))
		} else {
			capture.diffs = append(capture.diffs, diffResult{
				Path: relative, Kind: kind, Status: "unavailable", Truncated: true,
				Message: "one or both file versions were not captured within the diff limits",
			})
		}
		if lsp.SupportsPath(change.Path) && len(capture.set.Changes) < maxDiagnosedChanges {
			if kind != lsp.ChangeDeleted {
				info, err := os.Stat(change.Path)
				if err != nil || info.Size() > maxDiagnosticSourceBytes {
					capture.set.Truncated = true
					capture.set.Message = joinCaptureMessage(capture.set.Message, "changed source file is unavailable or too large for diagnostics: "+relative)
					continue
				}
				content, err := os.ReadFile(change.Path)
				if err != nil {
					capture.set.Truncated = true
					capture.set.Message = joinCaptureMessage(capture.set.Message, "read changed source file: "+err.Error())
					continue
				}
				change.Content = content
				if change.Revision == "" {
					digest := sha256.Sum256(content)
					change.Revision = hex.EncodeToString(digest[:])
				}
			}
			capture.set.Changes = append(capture.set.Changes, change)
		}
	}
	if len(capture.reported) > maxReportedChanges {
		capture.reported = capture.reported[:maxReportedChanges]
		capture.set.Truncated = true
		capture.set.Message = joinCaptureMessage(capture.set.Message, fmt.Sprintf("reported changes limited to %d files", maxReportedChanges))
	}
	supported := 0
	for _, change := range capture.reported {
		if lsp.SupportsPath(change.Path) {
			supported++
		}
	}
	if supported > maxDiagnosedChanges {
		capture.set.Truncated = true
		capture.set.Message = joinCaptureMessage(capture.set.Message, fmt.Sprintf("language diagnostics limited to %d changed source files", maxDiagnosedChanges))
	}
	return capture
}

func sameStamp(left, right fileStamp) bool {
	return left.size == right.size && left.modifiedNS == right.modifiedNS && left.mode == right.mode && left.revision == right.revision
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

func snapshotAfterMutation(root string) workspaceSnapshot {
	snapshot, err := snapshotWorkspace(context.Background(), root)
	if err != nil {
		return workspaceSnapshot{files: make(map[string]fileStamp), truncated: true, message: err.Error()}
	}
	return snapshot
}

package workspacecoord

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

const (
	ActionWrite  = "write"
	ActionDelete = "delete"

	applyPlanVersion = 1
)

// ErrApplyDiverged reports that the main workspace no longer matches what
// an apply plan expects. The commit stops without overwriting the path.
var ErrApplyDiverged = errors.New("main workspace diverged from the apply plan")

type (
	// PlanEntry changes one main-workspace path. A nil Preimage means the
	// path must be absent; a nil Postimage means it is deleted.
	PlanEntry struct {
		Path      string         `json:"path"`
		Action    string         `json:"action"`
		Preimage  *ManifestEntry `json:"preimage,omitempty"`
		Postimage *ManifestEntry `json:"postimage,omitempty"`
		Artifact  string         `json:"artifact,omitempty"`
		Merged    bool           `json:"merged,omitempty"`
	}

	// PlanConflict is a path the plan cannot change without a resolution.
	PlanConflict struct {
		Path   string `json:"path"`
		Reason string `json:"reason"`
	}

	// ApplyPlan is the exact set of main-workspace writes for one merge.
	// Approval binds to Digest; a commit rebuilds the plan under the
	// barrier and requires the same digest.
	ApplyPlan struct {
		Version         int                   `json:"version"`
		MergeID         orchestration.MergeID `json:"merge_id"`
		CandidateDigest string                `json:"candidate_digest"`
		Entries         []PlanEntry           `json:"entries"`
		Conflicts       []PlanConflict        `json:"conflicts,omitempty"`
		Digest          string                `json:"digest"`
	}

	// PlanInput names the merge, its candidate and the main workspace.
	PlanInput struct {
		MergeID       orchestration.MergeID
		Candidate     Candidate
		WorkspaceRoot string
		Artifacts     ArtifactStore
	}
)

// Paths lists the paths the plan writes or deletes.
func (plan ApplyPlan) Paths() []string {
	paths := make([]string, 0, len(plan.Entries))
	for _, entry := range plan.Entries {
		paths = append(paths, entry.Path)
	}
	return paths
}

// computeDigest hashes everything except the digest field.
func (plan ApplyPlan) computeDigest() string {
	plan.Digest = ""
	encoded, _ := json.Marshal(plan)
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// BuildApplyPlan compares each candidate path with current main. Paths main
// left at the baseline fast-forward; paths both sides changed are merged by
// Merge3 or reported as conflicts. Merged contents are stored as artifacts.
func BuildApplyPlan(ctx context.Context, input PlanInput) (ApplyPlan, error) {
	if input.Artifacts == nil {
		return ApplyPlan{}, errors.New("apply plan artifact storage is unavailable")
	}
	plan := ApplyPlan{Version: applyPlanVersion, MergeID: input.MergeID, CandidateDigest: input.Candidate.Digest}
	for _, change := range input.Candidate.Entries {
		if err := contextError(ctx); err != nil {
			return ApplyPlan{}, err
		}
		entry, conflict, err := planPath(ctx, input, change)
		if err != nil {
			return ApplyPlan{}, err
		}
		switch {
		case conflict != "":
			plan.Conflicts = append(plan.Conflicts, PlanConflict{Path: change.Path, Reason: conflict})
		case entry != nil:
			plan.Entries = append(plan.Entries, *entry)
		}
	}
	sort.Slice(plan.Entries, func(i, j int) bool { return plan.Entries[i].Path < plan.Entries[j].Path })
	plan.Digest = plan.computeDigest()
	return plan, nil
}

// planPath decides one candidate path. It returns nil without a conflict
// when main already holds the candidate's version.
func planPath(ctx context.Context, input PlanInput, change CandidateEntry) (*PlanEntry, string, error) {
	if reason := checkParents(input.WorkspaceRoot, change.Path); reason != "" {
		return nil, reason, nil
	}
	current, present, err := readEntry(input.WorkspaceRoot, change.Path)
	if errors.Is(err, errUnsupportedEntry) {
		return nil, "main has a directory at this path", nil
	}
	if err != nil {
		return nil, "", err
	}
	newPresent := change.Change != ChangeDelete
	basePresent := change.Change != ChangeAdd
	if present == newPresent && (!present || current.Same(change.New)) {
		return nil, "", nil
	}
	entry := &PlanEntry{Path: change.Path, Action: ActionDelete}
	if present {
		entry.Preimage = &current
	}
	if newPresent {
		entry.Action = ActionWrite
		postimage := change.New
		entry.Postimage, entry.Artifact = &postimage, change.Artifact
	}
	if present == basePresent && (!present || current.Same(change.Base)) {
		if newPresent && entry.Postimage.Type == EntryFile {
			if reason, err := markerConflict(ctx, input.Artifacts, change.Artifact); err != nil || reason != "" {
				return nil, reason, err
			}
		}
		return entry, "", nil
	}
	// Both sides changed the path. Only file contents can be merged.
	if !present || !newPresent || current.Type != EntryFile || change.New.Type != EntryFile ||
		(basePresent && change.Base.Type != EntryFile) {
		return nil, "main and candidate both changed this path", nil
	}
	return mergePath(ctx, input, change, current, entry)
}

// mergePath runs Merge3 for a path both sides changed and stores the result.
func mergePath(ctx context.Context, input PlanInput, change CandidateEntry, current ManifestEntry, entry *PlanEntry) (*PlanEntry, string, error) {
	var base []byte
	if change.BaseArtifact != "" {
		var err error
		if base, err = input.Artifacts.ReadArtifact(ctx, change.BaseArtifact); err != nil {
			return nil, "", fmt.Errorf("read baseline of %q: %w", change.Path, err)
		}
	}
	ours, err := os.ReadFile(filepath.Join(input.WorkspaceRoot, filepath.FromSlash(change.Path)))
	if err != nil {
		return nil, "", fmt.Errorf("read main %q: %w", change.Path, err)
	}
	theirs, err := input.Artifacts.ReadArtifact(ctx, change.Artifact)
	if err != nil {
		return nil, "", fmt.Errorf("read candidate %q: %w", change.Path, err)
	}
	if HasConflictMarkers(string(theirs)) {
		return nil, "candidate still contains conflict markers", nil
	}
	result, err := Merge3(string(base), string(ours), string(theirs), string(input.MergeID))
	if err != nil {
		return nil, "", err
	}
	if result.Binary {
		return nil, "main and candidate both changed binary content", nil
	}
	if result.Conflicts > 0 {
		return nil, fmt.Sprintf("%d overlapping edit region(s)", result.Conflicts), nil
	}
	reference, err := input.Artifacts.Put(ctx, []byte(result.Text))
	if err != nil {
		return nil, "", fmt.Errorf("store merged %q: %w", change.Path, err)
	}
	// The candidate's mode wins when it changed; otherwise main keeps its own.
	mode := current.Mode
	if change.Base.Type == EntryFile && change.New.Mode != change.Base.Mode {
		mode = change.New.Mode
	}
	entry.Postimage = &ManifestEntry{
		Path: change.Path, Type: EntryFile, Mode: mode,
		Size: int64(len(result.Text)), SHA256: hashBytes([]byte(result.Text)),
	}
	entry.Artifact, entry.Merged = reference, true
	return entry, "", nil
}

// markerConflict rejects candidate text that still holds merge markers.
func markerConflict(ctx context.Context, artifacts ArtifactReader, reference string) (string, error) {
	content, err := artifacts.ReadArtifact(ctx, reference)
	if err != nil {
		return "", fmt.Errorf("read candidate content: %w", err)
	}
	if isText(string(content)) && HasConflictMarkers(string(content)) {
		return "candidate still contains conflict markers", nil
	}
	return "", nil
}

// checkParents rejects paths whose parent components in main are symlinks or
// files, so a write can never follow a link out of the workspace.
func checkParents(root, relative string) string {
	parts := strings.Split(relative, "/")
	current := root
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return ""
		}
		if err != nil {
			return "cannot inspect parent directory: " + err.Error()
		}
		if !info.IsDir() {
			return "a parent of this path is not a directory in main"
		}
	}
	return ""
}

// VerifyPreimages reports whether main still matches every preimage of the
// plan. A commit holds the barrier while it calls this.
func VerifyPreimages(root string, plan ApplyPlan) error {
	for _, entry := range plan.Entries {
		if err := matchesImage(root, entry.Path, entry.Preimage); err != nil {
			return err
		}
	}
	return nil
}

// matchesImage checks one path against an expected image; nil means absent.
func matchesImage(root, relative string, image *ManifestEntry) error {
	current, present, err := readEntry(root, relative)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrApplyDiverged, err)
	}
	switch {
	case image == nil && !present:
		return nil
	case image != nil && present && current.Same(*image):
		return nil
	default:
		return fmt.Errorf("%w: %s", ErrApplyDiverged, relative)
	}
}

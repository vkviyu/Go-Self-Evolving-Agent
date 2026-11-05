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

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

const (
	ChangeAdd    = "add"
	ChangeModify = "modify"
	ChangeDelete = "delete"

	candidateVersion = 1
)

type (
	// ArtifactStore stores and reads content-addressed artifacts.
	ArtifactStore interface {
		ArtifactWriter
		ArtifactReader
	}

	// CandidateEntry is one changed path of a frozen work tree. Base is the
	// zero entry for an addition and New is the zero entry for a deletion.
	// Artifacts hold file contents; symlink targets live in the entries.
	CandidateEntry struct {
		Path         string        `json:"path"`
		Change       string        `json:"change"`
		Base         ManifestEntry `json:"base,omitzero"`
		New          ManifestEntry `json:"new,omitzero"`
		BaseArtifact string        `json:"base_artifact,omitempty"`
		Artifact     string        `json:"artifact,omitempty"`
	}

	// Candidate is the frozen output of a snapshot view relative to its
	// baseline. It does not depend on the view directory after creation.
	Candidate struct {
		Version    int                           `json:"version"`
		ViewID     orchestration.WorkspaceViewID `json:"view_id"`
		BaseDigest string                        `json:"base_digest"`
		WorkDigest string                        `json:"work_digest"`
		Entries    []CandidateEntry              `json:"entries"`
		Digest     string                        `json:"digest"`
	}
)

// CandidateDigest identifies the change from a baseline to a work tree.
func CandidateDigest(baseDigest, workDigest string) string {
	sum := sha256.Sum256([]byte("gsea-candidate-v1\n" + baseDigest + "\n" + workDigest))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// WorkManifest hashes the current work tree of a snapshot.
func WorkManifest(ctx context.Context, snapshot Snapshot) (Manifest, error) {
	manifest, err := BuildManifest(ctx, snapshot.WorkPath, nil)
	if err != nil {
		return Manifest{}, fmt.Errorf("hash snapshot work tree: %w", err)
	}
	return manifest, nil
}

// BuildCandidate freezes the changes of a snapshot work tree. It returns
// false when the work tree has the same content as its baseline.
func BuildCandidate(ctx context.Context, snapshot Snapshot, viewID orchestration.WorkspaceViewID, artifacts ArtifactWriter) (Candidate, bool, error) {
	if artifacts == nil {
		return Candidate{}, false, errors.New("candidate artifact storage is unavailable")
	}
	work, err := WorkManifest(ctx, snapshot)
	if err != nil {
		return Candidate{}, false, err
	}
	candidate := Candidate{
		Version: candidateVersion, ViewID: viewID,
		BaseDigest: snapshot.Digest, WorkDigest: work.Digest(),
	}
	if candidate.BaseDigest == candidate.WorkDigest {
		return candidate, false, nil
	}
	candidate.Digest = CandidateDigest(candidate.BaseDigest, candidate.WorkDigest)
	for _, change := range diffManifests(snapshot.Manifest, work) {
		if change.Base.Type == EntryFile {
			change.BaseArtifact, err = putFile(ctx, artifacts, snapshot.BasePath, change.Path)
			if err != nil {
				return Candidate{}, false, err
			}
		}
		if change.New.Type == EntryFile {
			change.Artifact, err = putFile(ctx, artifacts, snapshot.WorkPath, change.Path)
			if err != nil {
				return Candidate{}, false, err
			}
		}
		candidate.Entries = append(candidate.Entries, change)
	}
	return candidate, true, nil
}

// diffManifests lists the paths whose entries differ, sorted by path.
func diffManifests(base, work Manifest) []CandidateEntry {
	var changes []CandidateEntry
	i, j := 0, 0
	for i < len(base.Entries) || j < len(work.Entries) {
		switch {
		case j == len(work.Entries) || (i < len(base.Entries) && base.Entries[i].Path < work.Entries[j].Path):
			changes = append(changes, CandidateEntry{Path: base.Entries[i].Path, Change: ChangeDelete, Base: base.Entries[i]})
			i++
		case i == len(base.Entries) || work.Entries[j].Path < base.Entries[i].Path:
			changes = append(changes, CandidateEntry{Path: work.Entries[j].Path, Change: ChangeAdd, New: work.Entries[j]})
			j++
		default:
			if !base.Entries[i].Same(work.Entries[j]) {
				changes = append(changes, CandidateEntry{Path: work.Entries[j].Path, Change: ChangeModify, Base: base.Entries[i], New: work.Entries[j]})
			}
			i++
			j++
		}
	}
	return changes
}

// putFile stores the content of root/relative as an artifact.
func putFile(ctx context.Context, artifacts ArtifactWriter, root, relative string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil {
		return "", fmt.Errorf("read candidate file %q: %w", relative, err)
	}
	reference, err := artifacts.Put(ctx, data)
	if err != nil {
		return "", fmt.Errorf("store candidate file %q: %w", relative, err)
	}
	return reference, nil
}

// StoreCandidate writes the candidate document as an artifact.
func StoreCandidate(ctx context.Context, artifacts ArtifactWriter, candidate Candidate) (string, error) {
	encoded, err := json.Marshal(candidate)
	if err != nil {
		return "", err
	}
	return artifacts.Put(ctx, encoded)
}

// LoadCandidate reads and checks a candidate document.
func LoadCandidate(ctx context.Context, artifacts ArtifactReader, reference string) (Candidate, error) {
	if artifacts == nil {
		return Candidate{}, errors.New("candidate artifact reader is unavailable")
	}
	encoded, err := artifacts.ReadArtifact(ctx, reference)
	if err != nil {
		return Candidate{}, fmt.Errorf("read candidate: %w", err)
	}
	var candidate Candidate
	if err := json.Unmarshal(encoded, &candidate); err != nil {
		return Candidate{}, fmt.Errorf("decode candidate: %w", err)
	}
	if candidate.Version != candidateVersion {
		return Candidate{}, fmt.Errorf("unsupported candidate version %d", candidate.Version)
	}
	if candidate.Digest != CandidateDigest(candidate.BaseDigest, candidate.WorkDigest) {
		return Candidate{}, errors.New("candidate digest does not match its manifests")
	}
	return candidate, nil
}

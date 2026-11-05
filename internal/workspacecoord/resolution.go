package workspacecoord

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// PrepareResolution returns a snapshot Prepare hook that replays a
// conflicted candidate onto a fresh baseline of main. Paths main left at the
// candidate's baseline take the candidate's version. Files both sides
// changed get a Merge3 result with conflict markers for the resolver. Other
// paths keep main's version; the conflict reason lists them.
func PrepareResolution(ctx context.Context, candidate Candidate, artifacts ArtifactReader) func(work string, base Manifest) error {
	return func(work string, base Manifest) error {
		if artifacts == nil {
			return errors.New("candidate artifact reader is unavailable")
		}
		for _, change := range candidate.Entries {
			if err := contextError(ctx); err != nil {
				return err
			}
			if checkParents(work, change.Path) != "" {
				continue
			}
			if err := replayChange(ctx, work, base, change, string(candidate.ViewID), artifacts); err != nil {
				return fmt.Errorf("replay %q: %w", change.Path, err)
			}
		}
		return nil
	}
}

// replayChange applies one candidate change to the resolution work tree.
func replayChange(ctx context.Context, work string, base Manifest, change CandidateEntry, label string, artifacts ArtifactReader) error {
	current, present := base.Lookup(change.Path)
	basePresent := change.Change != ChangeAdd
	newPresent := change.Change != ChangeDelete
	path := filepath.Join(work, filepath.FromSlash(change.Path))
	if info, err := os.Lstat(path); err == nil && info.IsDir() {
		// Main has a directory here; the resolver decides by hand.
		return nil
	}
	if present == basePresent && (!present || current.Same(change.Base)) {
		if !newPresent {
			return removeEntry(path)
		}
		return writeImage(ctx, path, change.New, change.Artifact, artifacts)
	}
	mergeable := present && newPresent && current.Type == EntryFile && change.New.Type == EntryFile &&
		(!basePresent || change.Base.Type == EntryFile)
	if !mergeable {
		return nil
	}
	var baseText []byte
	if change.BaseArtifact != "" {
		var err error
		if baseText, err = artifacts.ReadArtifact(ctx, change.BaseArtifact); err != nil {
			return err
		}
	}
	ours, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	theirs, err := artifacts.ReadArtifact(ctx, change.Artifact)
	if err != nil {
		return err
	}
	result, err := Merge3(string(baseText), string(ours), string(theirs), label)
	if err != nil || result.Binary {
		return err
	}
	return writeFileSync(path, []byte(result.Text), os.FileMode(current.Mode))
}

// writeImage replaces path with a file or symlink image.
func writeImage(ctx context.Context, path string, image ManifestEntry, reference string, artifacts ArtifactReader) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := removeEntry(path); err != nil {
		return err
	}
	if image.Type == EntrySymlink {
		return os.Symlink(image.Target, path)
	}
	content, err := artifacts.ReadArtifact(ctx, reference)
	if err != nil {
		return err
	}
	if err := writeFileSync(path, content, os.FileMode(image.Mode)); err != nil {
		return err
	}
	return os.Chmod(path, os.FileMode(image.Mode))
}

// removeEntry deletes a file or symlink; a missing path is fine.
func removeEntry(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

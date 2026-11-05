package workspacecoord

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

const (
	ApplyStarted  ApplyState = "started"
	ApplyFinished ApplyState = "finished"

	// Commit steps reported to a CommitHook.
	StepJournaled = "journaled"
	StepWritten   = "written"
	StepRecorded  = "recorded"
	StepVerified  = "verified"
)

type (
	ApplyState string

	// ApplyRecord is the durable journal of one merge commit. Applied lists
	// the paths whose postimage was written and recorded.
	ApplyRecord struct {
		MergeID orchestration.MergeID
		Plan    ApplyPlan
		State   ApplyState
		Applied map[string]bool
	}

	// ApplyJournal persists commit progress before main is changed, so a
	// crash at any step can be completed or stopped safely.
	ApplyJournal interface {
		// BeginApply records the plan. Repeating it with the same digest
		// returns the existing record; a different digest is an error.
		BeginApply(context.Context, ApplyPlan) (ApplyRecord, error)
		MarkPathApplied(context.Context, orchestration.MergeID, string) error
		FinishApply(context.Context, orchestration.MergeID) error
		LoadApply(context.Context, orchestration.MergeID) (ApplyRecord, bool, error)
	}

	// CommitHook observes commit steps. Tests return an error to simulate a
	// crash right after the step.
	CommitHook func(step, path string) error
)

// CommitPlan writes plan to root. The caller holds the barrier and has
// checked the preimages. Each path is committed by temp file and rename,
// then recorded. A path that matches neither image stops the commit with
// ErrApplyDiverged and is left untouched.
func CommitPlan(ctx context.Context, root string, plan ApplyPlan, artifacts ArtifactReader, journal ApplyJournal, hook CommitHook) error {
	if journal == nil {
		return errors.New("merge apply journal is unavailable")
	}
	if hook == nil {
		hook = func(string, string) error { return nil }
	}
	record, err := journal.BeginApply(ctx, plan)
	if err != nil {
		return fmt.Errorf("journal apply plan: %w", err)
	}
	if record.State == ApplyFinished {
		return nil
	}
	if err := hook(StepJournaled, ""); err != nil {
		return err
	}
	for _, entry := range record.Plan.Entries {
		if err := contextError(ctx); err != nil {
			return err
		}
		if err := commitEntry(ctx, root, record, entry, artifacts, journal, hook); err != nil {
			return err
		}
	}
	for _, entry := range record.Plan.Entries {
		if err := matchesImage(root, entry.Path, entry.Postimage); err != nil {
			return fmt.Errorf("verify committed path: %w", err)
		}
	}
	if err := hook(StepVerified, ""); err != nil {
		return err
	}
	return journal.FinishApply(ctx, record.MergeID)
}

// commitEntry brings one path to its postimage. A path already at the
// postimage is recorded without a write, which makes recovery idempotent.
func commitEntry(ctx context.Context, root string, record ApplyRecord, entry PlanEntry, artifacts ArtifactReader, journal ApplyJournal, hook CommitHook) error {
	if matchesImage(root, entry.Path, entry.Postimage) != nil {
		if record.Applied[entry.Path] {
			return fmt.Errorf("%w: %s changed after it was applied", ErrApplyDiverged, entry.Path)
		}
		if err := matchesImage(root, entry.Path, entry.Preimage); err != nil {
			return err
		}
		if err := writeEntry(ctx, root, record.MergeID, entry, artifacts); err != nil {
			return fmt.Errorf("apply %q: %w", entry.Path, err)
		}
		if err := hook(StepWritten, entry.Path); err != nil {
			return err
		}
	}
	if record.Applied[entry.Path] {
		return nil
	}
	if err := journal.MarkPathApplied(ctx, record.MergeID, entry.Path); err != nil {
		return fmt.Errorf("record applied path %q: %w", entry.Path, err)
	}
	return hook(StepRecorded, entry.Path)
}

// writeEntry replaces or deletes one path. Content is checked against the
// postimage hash before it reaches main.
func writeEntry(ctx context.Context, root string, mergeID orchestration.MergeID, entry PlanEntry, artifacts ArtifactReader) error {
	path := filepath.Join(root, filepath.FromSlash(entry.Path))
	if entry.Action == ActionDelete {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return syncDirectory(filepath.Dir(path))
	}
	if reason := checkParents(root, entry.Path); reason != "" {
		return fmt.Errorf("%w: %s", ErrApplyDiverged, reason)
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	// The temporary name is deterministic so recovery replaces leftovers.
	temporary := filepath.Join(directory, applyTempPrefix+hashBytes([]byte(string(mergeID) + "\x00" + entry.Path))[:16])
	if err := os.Remove(temporary); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	postimage := entry.Postimage
	switch postimage.Type {
	case EntrySymlink:
		if err := os.Symlink(postimage.Target, temporary); err != nil {
			return err
		}
	case EntryFile:
		if artifacts == nil {
			return errors.New("content artifact reader is unavailable")
		}
		content, err := artifacts.ReadArtifact(ctx, entry.Artifact)
		if err != nil {
			return fmt.Errorf("read content: %w", err)
		}
		if hashBytes(content) != postimage.SHA256 {
			return errors.New("content artifact does not match the planned postimage")
		}
		if err := writeFileSync(temporary, content, os.FileMode(postimage.Mode)); err != nil {
			_ = os.Remove(temporary)
			return err
		}
		if err := os.Chmod(temporary, os.FileMode(postimage.Mode)); err != nil {
			_ = os.Remove(temporary)
			return err
		}
	default:
		return fmt.Errorf("unsupported postimage type %q", postimage.Type)
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return syncDirectory(directory)
}

// syncDirectory makes a rename or removal in directory durable.
func syncDirectory(directory string) error {
	handle, err := os.Open(directory)
	if err != nil {
		return err
	}
	syncErr := handle.Sync()
	closeErr := handle.Close()
	return errors.Join(syncErr, closeErr)
}

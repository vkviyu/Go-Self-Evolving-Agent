package workspacecoord

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

const (
	// DefaultSnapshotCopyLimit bounds plain-copied bytes per snapshot, counting
	// both the baseline and the work tree. Copy-on-write clones are free.
	DefaultSnapshotCopyLimit int64 = 1 << 30

	snapshotWorkDirectory = "work"
	snapshotHostDirectory = "host"
	snapshotBaseDirectory = "base"
	snapshotManifestFile  = "manifest.json"
	snapshotNamePrefix    = "snap-"
)

var (
	// ErrSnapshotCopyLimit reports that a snapshot needed more plain-copied
	// bytes than the configured limit. Callers must fail instead of falling
	// back to unprotected shared writes.
	ErrSnapshotCopyLimit = errors.New("snapshot copy limit exceeded")

	errCloneUnsupported = errors.New("copy-on-write clone is unsupported")

	// cloneEntry is the clone primitive; tests replace it to force copies.
	cloneEntry = cloneFile
)

type (
	// Barrier pauses workspace writers that honor leases while a consistent
	// image of the main workspace is read or written.
	Barrier interface {
		Hold(ctx context.Context, teamID orchestration.TeamID, agentID orchestration.AgentID, runID orchestration.RunID) (func(), error)
	}

	// BarrierFunc adapts a function to Barrier.
	BarrierFunc func(ctx context.Context, teamID orchestration.TeamID, agentID orchestration.AgentID, runID orchestration.RunID) (func(), error)

	// SnapshotStore materializes private work trees below ViewsDirectory.
	// Each snapshot keeps an immutable baseline next to its work tree.
	SnapshotStore struct {
		ViewsDirectory string
		WorkspaceRoot  string
		CopyLimit      int64
		Exclude        []string
		Barrier        Barrier
	}

	// SnapshotRequest identifies the run that owns a snapshot. Prepare may edit
	// the private work tree before the snapshot becomes visible.
	SnapshotRequest struct {
		TeamID  orchestration.TeamID
		AgentID orchestration.AgentID
		RunID   orchestration.RunID
		Prepare func(work string, base Manifest) error
	}

	// Snapshot describes a materialized snapshot directory.
	Snapshot struct {
		Name     string
		Root     string
		WorkPath string
		BasePath string
		Manifest Manifest
		Digest   string
		Cloned   bool
	}

	// copyBudget tracks clone attempts and plain-copied bytes of one snapshot.
	copyBudget struct {
		limit  int64
		used   int64
		clone  bool
		copied bool
	}
)

// Hold calls the adapted function.
func (barrier BarrierFunc) Hold(ctx context.Context, teamID orchestration.TeamID, agentID orchestration.AgentID, runID orchestration.RunID) (func(), error) {
	return barrier(ctx, teamID, agentID, runID)
}

// SnapshotName returns the deterministic directory name for one run, so a
// retried start reuses the snapshot created by an interrupted attempt.
func SnapshotName(teamID orchestration.TeamID, agentID orchestration.AgentID, runID orchestration.RunID) string {
	sum := sha256.Sum256([]byte(string(teamID) + "/" + string(agentID) + "/" + string(runID)))
	return snapshotNamePrefix + hex.EncodeToString(sum[:])[:24]
}

// Materialize creates or reuses the snapshot of request's run. The baseline
// is copied under the barrier; the work tree is copied from the baseline
// afterwards. The directory is built under a temporary name and renamed.
func (store *SnapshotStore) Materialize(ctx context.Context, request SnapshotRequest) (Snapshot, error) {
	if err := store.validate(); err != nil {
		return Snapshot{}, err
	}
	name := SnapshotName(request.TeamID, request.AgentID, request.RunID)
	final := filepath.Join(store.ViewsDirectory, name)
	if snapshot, err := store.load(final); err == nil {
		return snapshot, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Snapshot{}, err
	}
	if err := os.MkdirAll(store.ViewsDirectory, 0o700); err != nil {
		return Snapshot{}, fmt.Errorf("create snapshot directory: %w", err)
	}
	temporary, err := os.MkdirTemp(store.ViewsDirectory, ".tmp-"+name+"-")
	if err != nil {
		return Snapshot{}, fmt.Errorf("create temporary snapshot: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(temporary)
		}
	}()
	cloned, err := store.copyImage(ctx, request, temporary)
	if err != nil {
		return Snapshot{}, err
	}
	base := filepath.Join(temporary, snapshotHostDirectory, snapshotBaseDirectory)
	manifest, err := BuildManifest(ctx, base, nil)
	if err != nil {
		return Snapshot{}, fmt.Errorf("hash snapshot baseline: %w", err)
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return Snapshot{}, err
	}
	if err := writeFileSync(filepath.Join(temporary, snapshotHostDirectory, snapshotManifestFile), encoded, 0o600); err != nil {
		return Snapshot{}, fmt.Errorf("write snapshot manifest: %w", err)
	}
	if request.Prepare != nil {
		if err := request.Prepare(filepath.Join(temporary, snapshotWorkDirectory), manifest); err != nil {
			return Snapshot{}, fmt.Errorf("prepare snapshot work tree: %w", err)
		}
	}
	if err := os.Rename(temporary, final); err != nil {
		// A concurrent attempt may have published the same run first.
		if snapshot, loadErr := store.load(final); loadErr == nil {
			return snapshot, nil
		}
		return Snapshot{}, fmt.Errorf("publish snapshot: %w", err)
	}
	keep = true
	snapshot, err := store.load(final)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot.Cloned = cloned
	return snapshot, nil
}

// copyImage copies the main workspace into the temporary baseline under the
// barrier, then copies the baseline into the work tree. It reports whether
// every file was cloned.
func (store *SnapshotStore) copyImage(ctx context.Context, request SnapshotRequest, temporary string) (bool, error) {
	base := filepath.Join(temporary, snapshotHostDirectory, snapshotBaseDirectory)
	work := filepath.Join(temporary, snapshotWorkDirectory)
	for _, directory := range []string{base, work} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return false, err
		}
	}
	limit := store.CopyLimit
	if limit <= 0 {
		limit = DefaultSnapshotCopyLimit
	}
	budget := &copyBudget{limit: limit, clone: true}
	exclude := store.exclusions()
	release := func() {}
	if store.Barrier != nil {
		var err error
		release, err = store.Barrier.Hold(ctx, request.TeamID, request.AgentID, request.RunID)
		if err != nil {
			return false, fmt.Errorf("pause workspace writers for snapshot: %w", err)
		}
	}
	err := copyTree(ctx, store.WorkspaceRoot, base, exclude, budget)
	baseCloned := err == nil && !budget.copied
	if baseCloned {
		// Version control metadata is only worth carrying when it is free.
		// A failed clone leaves the snapshot without it.
		if gitErr := cloneGitMetadata(ctx, store.WorkspaceRoot, work); gitErr != nil {
			_ = os.RemoveAll(filepath.Join(work, ".git"))
		}
	}
	release()
	if err != nil {
		return false, fmt.Errorf("copy workspace baseline: %w", err)
	}
	budget.clone, budget.copied = true, false
	if err := copyTree(ctx, base, work, nil, budget); err != nil {
		return false, fmt.Errorf("copy snapshot work tree: %w", err)
	}
	return baseCloned && !budget.copied, nil
}

// exclusions adds the views directory when it lives inside the workspace, so
// a snapshot never copies other snapshots.
func (store *SnapshotStore) exclusions() []string {
	exclude := append([]string(nil), store.Exclude...)
	if relative, err := filepath.Rel(store.WorkspaceRoot, store.ViewsDirectory); err == nil && relative != "." && filepath.IsLocal(relative) {
		exclude = append(exclude, filepath.ToSlash(relative))
	}
	return exclude
}

// validate checks that the store has absolute, distinct directories.
func (store *SnapshotStore) validate() error {
	if store == nil {
		return errors.New("snapshot store is unavailable")
	}
	if !filepath.IsAbs(store.ViewsDirectory) || !filepath.IsAbs(store.WorkspaceRoot) {
		return errors.New("snapshot store requires absolute views and workspace directories")
	}
	if filepath.Clean(store.ViewsDirectory) == filepath.Clean(store.WorkspaceRoot) {
		return errors.New("snapshot views directory must differ from the workspace root")
	}
	return nil
}

// ForView loads the snapshot that backs a snapshot workspace view.
func (store *SnapshotStore) ForView(view View) (Snapshot, error) {
	root, err := store.viewSnapshotRoot(view)
	if err != nil {
		return Snapshot{}, err
	}
	return store.load(root)
}

// Remove deletes the snapshot directory behind a view. Callers decide first
// that the view holds no undelivered work.
func (store *SnapshotStore) Remove(view View) error {
	root, err := store.viewSnapshotRoot(view)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("remove snapshot %s: %w", filepath.Base(root), err)
	}
	return nil
}

// viewSnapshotRoot maps a view's work path to its snapshot directory and
// rejects paths that are not snapshots of this store.
func (store *SnapshotStore) viewSnapshotRoot(view View) (string, error) {
	if store == nil {
		return "", errors.New("snapshot store is unavailable")
	}
	if view.Mode != ViewSnapshot {
		return "", fmt.Errorf("%w: view %s is not a snapshot view", orchestration.ErrInvalidEvent, view.ID)
	}
	work := filepath.Clean(view.RootPath)
	root := filepath.Dir(work)
	if filepath.Base(work) != snapshotWorkDirectory || filepath.Dir(root) != filepath.Clean(store.ViewsDirectory) ||
		!strings.HasPrefix(filepath.Base(root), snapshotNamePrefix) {
		return "", fmt.Errorf("%w: view %s is outside the snapshot directory", orchestration.ErrInvalidEvent, view.ID)
	}
	return root, nil
}

// load reads a published snapshot. A missing manifest reports os.ErrNotExist.
func (store *SnapshotStore) load(root string) (Snapshot, error) {
	encoded, err := os.ReadFile(filepath.Join(root, snapshotHostDirectory, snapshotManifestFile))
	if err != nil {
		return Snapshot{}, err
	}
	var manifest Manifest
	if err := json.Unmarshal(encoded, &manifest); err != nil {
		return Snapshot{}, fmt.Errorf("decode snapshot manifest: %w", err)
	}
	if manifest.Version != manifestVersion {
		return Snapshot{}, fmt.Errorf("unsupported snapshot manifest version %d", manifest.Version)
	}
	work := filepath.Join(root, snapshotWorkDirectory)
	if info, err := os.Stat(work); err != nil || !info.IsDir() {
		return Snapshot{}, fmt.Errorf("snapshot work tree is missing: %w", errors.Join(err, os.ErrNotExist))
	}
	return Snapshot{
		Name: filepath.Base(root), Root: root, WorkPath: work,
		BasePath: filepath.Join(root, snapshotHostDirectory, snapshotBaseDirectory),
		Manifest: manifest, Digest: manifest.Digest(),
	}, nil
}

// copyTree copies the kept entries of src into dst, creating parents.
func copyTree(ctx context.Context, src, dst string, exclude []string, budget *copyBudget) error {
	return walkTree(ctx, src, exclude, func(relative string, _ fs.DirEntry) error {
		target := filepath.Join(dst, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return copyEntry(filepath.Join(src, filepath.FromSlash(relative)), target, budget)
	})
}

// copyEntry clones or copies one file or symlink. The first failed clone
// switches the rest of the tree to plain copy, which counts against budget.
func copyEntry(from, to string, budget *copyBudget) error {
	info, err := os.Lstat(from)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(from)
		if err != nil {
			return err
		}
		return os.Symlink(target, to)
	case info.Mode().IsRegular():
		if budget.clone {
			if cloneEntry(from, to) == nil {
				return os.Chmod(to, info.Mode().Perm())
			}
			budget.clone = false
			_ = os.Remove(to)
		}
		budget.used += info.Size()
		if budget.used > budget.limit {
			return fmt.Errorf("%w: more than %d bytes need a plain copy", ErrSnapshotCopyLimit, budget.limit)
		}
		budget.copied = true
		return copyFile(from, to, info.Mode().Perm())
	default:
		return nil
	}
}

// cloneGitMetadata clones root/.git into work/.git. Any entry that cannot be
// cloned fails the whole copy; the caller then drops the partial metadata.
func cloneGitMetadata(ctx context.Context, root, work string) error {
	source := filepath.Join(root, ".git")
	if _, err := os.Lstat(source); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := contextError(ctx); err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		target := filepath.Join(work, relative)
		switch {
		case entry.IsDir():
			return os.MkdirAll(target, 0o755)
		case entry.Type()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case entry.Type().IsRegular():
			return cloneEntry(path, target)
		default:
			return nil
		}
	})
}

// copyFile copies a regular file to a new destination with perm.
func copyFile(from, to string, perm os.FileMode) error {
	source, err := os.Open(from)
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(target, source)
	closeErr := target.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return err
	}
	return os.Chmod(to, perm)
}

// writeFileSync writes data to a new or truncated file and syncs it.
func writeFileSync(path string, data []byte, perm os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	return errors.Join(writeErr, syncErr, closeErr)
}

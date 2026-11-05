package workspacecoord

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

const (
	// EntryFile and EntrySymlink are the only entry types a snapshot carries.
	// Other file types are skipped because they cannot be merged safely.
	EntryFile    = "file"
	EntrySymlink = "symlink"

	manifestVersion = 1
	// Modes are normalized to the executable bit so umask differences never
	// look like content changes.
	modeRegular    uint32 = 0o644
	modeExecutable uint32 = 0o755
	// applyTempPrefix names the temporary files a merge commit writes next to
	// their destination. Manifests skip them so an interrupted commit is never
	// mistaken for user content.
	applyTempPrefix = ".gsea-apply-"
)

type (
	// ManifestEntry identifies one file or symlink by content. SHA256 is the
	// file content hash, or the hash of the link target for symlinks.
	ManifestEntry struct {
		Path   string `json:"path"`
		Type   string `json:"type"`
		Mode   uint32 `json:"mode,omitempty"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
		Target string `json:"target,omitempty"`
	}

	// Manifest lists the entries of a tree sorted by path.
	Manifest struct {
		Version int             `json:"version"`
		Entries []ManifestEntry `json:"entries"`
	}
)

// Same reports whether two entries have the same type, mode and content.
func (entry ManifestEntry) Same(other ManifestEntry) bool {
	return entry.Type == other.Type && entry.Mode == other.Mode &&
		entry.SHA256 == other.SHA256 && entry.Target == other.Target
}

// Digest returns a stable content identity for the whole tree.
func (manifest Manifest) Digest() string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "gsea-manifest-v%d\n", manifestVersion)
	for _, entry := range manifest.Entries {
		_, _ = fmt.Fprintf(hash, "%s\x00%s\x00%o\x00%s\x00%s\n", entry.Path, entry.Type, entry.Mode, entry.SHA256, entry.Target)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

// Lookup finds the entry for a slash-separated relative path.
func (manifest Manifest) Lookup(path string) (ManifestEntry, bool) {
	index := sort.Search(len(manifest.Entries), func(i int) bool { return manifest.Entries[i].Path >= path })
	if index < len(manifest.Entries) && manifest.Entries[index].Path == path {
		return manifest.Entries[index], true
	}
	return ManifestEntry{}, false
}

// BuildManifest hashes every regular file and symlink below root. Version
// control metadata, runtime state and the exclude prefixes are skipped.
func BuildManifest(ctx context.Context, root string, exclude []string) (Manifest, error) {
	manifest := Manifest{Version: manifestVersion}
	err := walkTree(ctx, root, exclude, func(relative string, entry fs.DirEntry) error {
		item, ok, err := readEntry(root, relative)
		if err != nil || !ok {
			return err
		}
		manifest.Entries = append(manifest.Entries, item)
		return nil
	})
	if err != nil {
		return Manifest{}, err
	}
	sort.Slice(manifest.Entries, func(i, j int) bool { return manifest.Entries[i].Path < manifest.Entries[j].Path })
	return manifest, nil
}

// walkTree visits every non-directory entry below root that the snapshot
// rules keep, passing its slash-separated relative path.
func walkTree(ctx context.Context, root string, exclude []string, visit func(string, fs.DirEntry) error) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := contextError(ctx); err != nil {
			return err
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if excludedPath(relative, exclude) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		return visit(relative, entry)
	})
}

// excludedPath reports whether a relative path is outside snapshot scope.
// Any ".git" component is excluded so nested repositories stay untouched.
func excludedPath(relative string, exclude []string) bool {
	parts := strings.Split(relative, "/")
	if slices.Contains(parts, ".git") {
		return true
	}
	if strings.HasPrefix(parts[len(parts)-1], applyTempPrefix) {
		return true
	}
	if relative == ".gsea" || strings.HasPrefix(relative, ".gsea/") {
		return true
	}
	for _, prefix := range exclude {
		prefix = strings.Trim(filepath.ToSlash(prefix), "/")
		if prefix != "" && (relative == prefix || strings.HasPrefix(relative, prefix+"/")) {
			return true
		}
	}
	return false
}

// readEntry describes one path below root. It returns false for a missing
// path and an error for a directory or special file at the path.
func readEntry(root, relative string) (ManifestEntry, bool, error) {
	path := filepath.Join(root, filepath.FromSlash(relative))
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return ManifestEntry{}, false, nil
	}
	if err != nil {
		return ManifestEntry{}, false, fmt.Errorf("inspect %q: %w", relative, err)
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return ManifestEntry{}, false, fmt.Errorf("read symlink %q: %w", relative, err)
		}
		return ManifestEntry{Path: relative, Type: EntrySymlink, Size: int64(len(target)), SHA256: hashBytes([]byte(target)), Target: target}, true, nil
	case info.Mode().IsRegular():
		sum, size, err := hashFile(path)
		if err != nil {
			return ManifestEntry{}, false, fmt.Errorf("hash %q: %w", relative, err)
		}
		return ManifestEntry{Path: relative, Type: EntryFile, Mode: normalizeMode(info.Mode()), Size: size, SHA256: sum}, true, nil
	case info.IsDir():
		return ManifestEntry{}, false, fmt.Errorf("%w: %q is a directory", errUnsupportedEntry, relative)
	default:
		return ManifestEntry{}, false, nil
	}
}

var errUnsupportedEntry = errors.New("unsupported workspace entry")

// normalizeMode keeps only whether any executable bit is set.
func normalizeMode(mode os.FileMode) uint32 {
	if mode.Perm()&0o111 != 0 {
		return modeExecutable
	}
	return modeRegular
}

// hashFile returns the hex sha256 and size of a regular file.
func hashFile(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

// hashBytes returns the hex sha256 of data.
func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

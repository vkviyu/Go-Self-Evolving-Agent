package codingtools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (workspace *Workspace) resolveExistingAt(root, path string) (string, string, error) {
	relative, err := normalizeRelative(path)
	if err != nil {
		return "", "", err
	}
	joined := filepath.Join(root, filepath.FromSlash(relative))
	real, err := filepath.EvalSymlinks(joined)
	if err != nil {
		return "", "", fmt.Errorf("resolve workspace path %q under %q: %w", relative, root, err)
	}
	if err := ensureInsideRoot(root, real); err != nil {
		return "", "", err
	}
	return real, relative, nil
}

func (workspace *Workspace) resolveParentAt(root, path string) (string, string, error) {
	relative, err := normalizeRelative(path)
	if err != nil {
		return "", "", err
	}
	joined := filepath.Join(root, filepath.FromSlash(relative))
	parent := filepath.Dir(joined)
	for {
		if _, err := os.Stat(parent); err == nil {
			parent, err = filepath.EvalSymlinks(parent)
			if err != nil {
				return "", "", fmt.Errorf("resolve parent for %q: %w", relative, err)
			}
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", "", fmt.Errorf("inspect parent for %q: %w", relative, err)
		}
		next := filepath.Dir(parent)
		if next == parent {
			return "", "", fmt.Errorf("resolve parent for %q: no existing workspace ancestor", relative)
		}
		parent = next
	}
	if err := ensureInsideRoot(root, parent); err != nil {
		return "", "", err
	}
	return joined, relative, nil
}

func ensureInsideRoot(root, path string) error {
	relative, err := filepath.Rel(root, path)
	if err != nil || !filepath.IsLocal(relative) {
		return fmt.Errorf("path %q escapes workspace root %q; use --workspace to select a different project", path, root)
	}
	return nil
}

func normalizeRelative(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return ".", nil
	}
	if strings.IndexByte(path, 0) >= 0 || filepath.IsAbs(path) {
		return "", errors.New("path must be workspace-relative")
	}
	path = filepath.ToSlash(filepath.Clean(path))
	if path == ".." || strings.HasPrefix(path, "../") {
		return "", errors.New("path escapes workspace")
	}
	return path, nil
}

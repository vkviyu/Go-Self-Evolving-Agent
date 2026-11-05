package workspacedomain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Identity names one workspace directory independently of the path used to
// reach it, so symlink aliases and bind mounts share a coordination domain.
type Identity struct {
	// ID is derived from the directory's device and inode numbers.
	ID string
	// Root is the canonical path observed when the identity was computed.
	Root string
}

// Identify resolves root to a canonical directory and derives its identity.
func Identify(root string) (Identity, error) {
	if strings.TrimSpace(root) == "" {
		return Identity{}, errors.New("workspace root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return Identity{}, fmt.Errorf("resolve workspace root: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return Identity{}, fmt.Errorf("resolve workspace root symlinks: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return Identity{}, fmt.Errorf("inspect workspace root: %w", err)
	}
	if !info.IsDir() {
		return Identity{}, fmt.Errorf("workspace root %q is not a directory", canonical)
	}
	digest := sha256.Sum256([]byte(directoryKey(canonical, info)))
	return Identity{ID: hex.EncodeToString(digest[:16]), Root: canonical}, nil
}

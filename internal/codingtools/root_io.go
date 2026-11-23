package codingtools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Root-relative handles keep symlink replacement between validation and I/O
// from reopening host paths. Limits apply to bytes actually read, not only stat.
func readRootBounded(ctx context.Context, root *os.Root, name string, limit int) ([]byte, bool, error) {
	if err := contextError(ctx); err != nil {
		return nil, false, err
	}
	file, _, err := openRootRegular(root, name)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, false, err
	}
	if err := contextError(ctx); err != nil {
		return nil, false, err
	}
	if len(data) > limit {
		return data[:limit], true, nil
	}
	return data, false, nil
}

func openRootRegular(root *os.Root, name string) (*os.File, os.FileInfo, error) {
	file, err := openRootFile(root, name)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("workspace content must be a regular file")
	}
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	return file, info, nil
}

func atomicWriteRoot(root *os.Root, name string, data []byte, mode os.FileMode) error {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	temporaryName := filepath.Join(filepath.Dir(name), ".gsea-write-"+hex.EncodeToString(nonce[:]))
	temporary, err := root.OpenFile(temporaryName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
	if err != nil {
		return err
	}
	defer root.Remove(temporaryName)
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Chmod(mode.Perm()); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := root.Rename(temporaryName, name); err != nil {
		return err
	}
	directory, err := root.Open(filepath.Dir(name))
	if err != nil {
		return fmt.Errorf("open changed directory: %w", err)
	}
	defer directory.Close()
	return directory.Sync()
}

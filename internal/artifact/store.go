// Package artifact owns bounded content-addressed objects. All I/O stays
// beneath an OS directory handle, including after symlink replacement.
package artifact

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const defaultMaxBytes int64 = 32 << 20

type Store struct {
	Root     string
	MaxBytes int64
}

func NewStore(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("artifact store root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0700); err != nil {
		return nil, err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	files, err := os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	if err := files.Close(); err != nil {
		return nil, err
	}
	return &Store{Root: abs, MaxBytes: defaultMaxBytes}, nil
}

func (store *Store) maxBytes() int64 {
	if store == nil || store.MaxBytes <= 0 {
		return defaultMaxBytes
	}
	return store.MaxBytes
}

func (store *Store) openRoot() (*os.Root, error) {
	if store == nil || strings.TrimSpace(store.Root) == "" {
		return nil, errors.New("artifact store is unavailable")
	}
	return os.OpenRoot(store.Root)
}

func (store *Store) Put(ctx context.Context, data []byte) (string, error) {
	if err := contextError(ctx); err != nil {
		return "", err
	}
	if int64(len(data)) > store.maxBytes() {
		return "", fmt.Errorf("artifact exceeds %d bytes", store.maxBytes())
	}
	files, err := store.openRoot()
	if err != nil {
		return "", err
	}
	defer files.Close()
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	reference, name := "sha256:"+digest, filepath.Join("sha256", digest)
	if err := files.MkdirAll("sha256", 0700); err != nil {
		return "", err
	}
	if _, err := files.Lstat(name); err == nil {
		if _, err := store.ReadArtifact(ctx, reference); err != nil {
			return "", fmt.Errorf("existing artifact failed integrity check: %w", err)
		}
		return reference, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	temporary := filepath.Join("sha256", ".artifact-"+hex.EncodeToString(nonce[:]))
	file, err := files.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	defer files.Remove(temporary)
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	if err := contextError(ctx); err != nil {
		return "", err
	}
	if err := files.Rename(temporary, name); err != nil {
		return "", err
	}
	directory, err := files.Open("sha256")
	if err != nil {
		return "", err
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return "", err
	}
	directoryRoot, err := files.Open(".")
	if err != nil {
		return "", err
	}
	defer directoryRoot.Close()
	if err := directoryRoot.Sync(); err != nil {
		return "", err
	}
	return reference, nil
}

func (store *Store) ReadArtifact(ctx context.Context, reference string) ([]byte, error) {
	data, _, err := store.ReadSegment(ctx, reference, 0, store.maxBytes())
	return data, err
}

// ReadSegment verifies the complete bounded object while retaining only the
// requested range. Returned bytes and the digest are from the same read pass.
func (store *Store) ReadSegment(ctx context.Context, reference string, offset, limit int64) ([]byte, int64, error) {
	if err := contextError(ctx); err != nil {
		return nil, 0, err
	}
	if offset < 0 || limit < 0 || limit > store.maxBytes() {
		return nil, 0, errors.New("invalid artifact segment")
	}
	name, err := normalizeReference(reference)
	if err != nil {
		return nil, 0, err
	}
	files, err := store.openRoot()
	if err != nil {
		return nil, 0, err
	}
	defer files.Close()
	file, err := files.Open(name)
	if err != nil {
		return nil, 0, fmt.Errorf("read artifact: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, errors.New("artifact must be a regular file")
	}
	if info.Size() > store.maxBytes() {
		return nil, 0, errors.New("artifact exceeds size limit")
	}
	digest := sha256.New()
	var result []byte
	var total int64
	buffer := make([]byte, 32<<10)
	for {
		if err := contextError(ctx); err != nil {
			return nil, 0, err
		}
		n, readErr := file.Read(buffer)
		if n > 0 {
			if total+int64(n) > store.maxBytes() {
				return nil, 0, errors.New("artifact exceeds size limit")
			}
			_, _ = digest.Write(buffer[:n])
			start := max(offset-total, 0)
			if start < int64(n) && int64(len(result)) < limit {
				end := min(int64(n), start+limit-int64(len(result)))
				result = append(result, buffer[start:end]...)
			}
			total += int64(n)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, 0, readErr
		}
	}
	if hex.EncodeToString(digest.Sum(nil)) != strings.TrimPrefix(reference, "sha256:") {
		return nil, 0, errors.New("artifact content hash does not match reference")
	}
	if offset > total {
		return nil, 0, fmt.Errorf("artifact offset %d exceeds content length %d", offset, total)
	}
	return result, total, nil
}

func normalizeReference(reference string) (string, error) {
	digest, ok := strings.CutPrefix(reference, "sha256:")
	if !ok || len(digest) != sha256.Size*2 {
		return "", errors.New("artifact reference must be sha256:<lowercase hex digest>")
	}
	decoded, err := hex.DecodeString(digest)
	if err != nil || hex.EncodeToString(decoded) != digest {
		return "", errors.New("invalid sha256 artifact reference")
	}
	return filepath.Join("sha256", digest), nil
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

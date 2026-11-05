package pluginbuild

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
)

const toolchainHashScope = "host-go+pkg-tool+stdlib-src+include+lib+config-v1"

var errToolchainChanged = errors.New("toolchain content fingerprint changed")

// fingerprintToolchain records actual driver/compiler/stdlib inputs, not just
// the go launcher. A trusted operator selects GOROOT; no model path is used.
// Before/after verification detects changes, but is not an immutable mount or
// a hard RSS quota and does not prove an attacker cannot swap/restore files.
func fingerprintToolchain(ctx context.Context, rootPath string) (string, error) {
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return "", err
	}
	defer root.Close()
	hash := sha256.New()
	fmt.Fprintln(hash, toolchainHashScope)
	for _, subtree := range []string{"bin/go", "pkg/tool", "pkg/include", "src", "lib", "go.env", "VERSION"} {
		if _, err := root.Lstat(subtree); errors.Is(err, fs.ErrNotExist) && (subtree == "lib" || subtree == "go.env") {
			fmt.Fprintln(hash, "absent", subtree)
			continue
		} else if err != nil {
			return "", err
		}
		err := fs.WalkDir(root.FS(), subtree, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			if !entry.Type().IsRegular() {
				return fmt.Errorf("unsupported non-regular toolchain entry %s", path)
			}
			file, err := root.Open(filepath.FromSlash(path))
			if err != nil {
				return err
			}
			before, err := file.Stat()
			if err != nil {
				_ = file.Close()
				return err
			}
			fmt.Fprintf(hash, "%s\x00%d\x00%d\x00", path, before.Size(), before.Mode().Perm())
			_, copyErr := io.Copy(hash, contextReader{Context: ctx, Reader: file})
			after, statErr := file.Stat()
			closeErr := file.Close()
			if err := errors.Join(copyErr, statErr, closeErr); err != nil {
				return err
			}
			if before.Size() != after.Size() || before.Mode() != after.Mode() || before.ModTime() != after.ModTime() {
				return errToolchainChanged
			}
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

type contextReader struct {
	context.Context
	io.Reader
}

func (reader contextReader) Read(buffer []byte) (int, error) {
	if err := reader.Context.Err(); err != nil {
		return 0, err
	}
	return reader.Reader.Read(buffer)
}

//go:build linux

package workspacecoord

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// cloneFile creates dst as a reflink clone of the regular file src. File
// systems without reflink support return an error and the caller copies.
func cloneFile(src, dst string) error {
	source, err := os.OpenFile(src, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return err
	}
	target, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	cloneErr := unix.IoctlFileClone(int(target.Fd()), int(source.Fd()))
	closeErr := target.Close()
	if cloneErr != nil || closeErr != nil {
		_ = os.Remove(dst)
		return errors.Join(cloneErr, closeErr)
	}
	return nil
}

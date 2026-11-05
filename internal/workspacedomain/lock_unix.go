//go:build darwin || linux

package workspacedomain

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// tryLock applies a non-blocking flock to this open file description.
// errLocked reports that another description holds a conflicting lock.
func tryLock(file *os.File, exclusive bool) error {
	how := unix.LOCK_SH
	if exclusive {
		how = unix.LOCK_EX
	}
	err := unix.Flock(int(file.Fd()), how|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return errLocked
	}
	return err
}

// unlock drops this description's flock without closing the file.
func unlock(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}

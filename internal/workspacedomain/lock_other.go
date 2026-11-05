//go:build !darwin && !linux

package workspacedomain

import (
	"errors"
	"os"
)

var errUnsupported = errors.New("workspace coordination locks are unavailable on this platform")

// tryLock is unavailable without flock; coordination fails closed.
func tryLock(*os.File, bool) error {
	return errUnsupported
}

// unlock is unavailable without flock; coordination fails closed.
func unlock(*os.File) error {
	return errUnsupported
}

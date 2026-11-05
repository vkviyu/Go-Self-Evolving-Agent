//go:build !darwin && !linux

package runtimeowner

import (
	"errors"
	"os"
)

func tryLock(*os.File) error {
	return errors.New("runtime ownership locking is unavailable on this platform")
}

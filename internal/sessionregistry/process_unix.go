//go:build darwin || linux

package sessionregistry

import (
	"errors"
	"syscall"
)

func processAlive(pid int) (bool, error) {
	if pid <= 0 {
		return true, nil // missing PID is not proof that a runtime has stopped
	}
	err := syscall.Kill(pid, 0)
	if err == nil || errors.Is(err, syscall.EPERM) {
		return true, nil
	}
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	return false, err
}

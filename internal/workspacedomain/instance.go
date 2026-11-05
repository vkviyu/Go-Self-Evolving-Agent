package workspacedomain

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const instancesDirectoryName = "instances"

// instanceLocks keep this process visible as a live lease holder. The
// process lock is held exclusively for the instance lifetime; descendant
// processes hold shared locks on a second file through inherited descriptors.
type instanceLocks struct {
	directory string
	id        string
	process   *os.File
}

// createInstanceLocks registers a fresh instance identity. O_EXCL guarantees
// that no other process has ever opened these files, so a probe that wins
// either lock can only observe a holder that has exited.
func createInstanceLocks(directory string) (*instanceLocks, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create instance lock directory: %w", err)
	}
	id, err := newInstanceID()
	if err != nil {
		return nil, err
	}
	process, err := os.OpenFile(processLockPath(directory, id), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create instance lock: %w", err)
	}
	if err := tryLock(process, true); err != nil {
		_ = process.Close()
		removeInstanceFiles(directory, id)
		return nil, fmt.Errorf("hold instance lock: %w", err)
	}
	descendants, err := os.OpenFile(descendantLockPath(directory, id), os.O_CREATE|os.O_EXCL|os.O_RDONLY, 0o600)
	if err != nil {
		_ = process.Close()
		removeInstanceFiles(directory, id)
		return nil, fmt.Errorf("create descendant lock: %w", err)
	}
	_ = descendants.Close()
	return &instanceLocks{directory: directory, id: id, process: process}, nil
}

// descendantLock opens a new description of the descendant lock with a
// shared hold. A child process that inherits it keeps this instance live
// after the instance itself dies; unlocking it affects only that child.
func (locks *instanceLocks) descendantLock() (*os.File, error) {
	file, err := os.Open(descendantLockPath(locks.directory, locks.id))
	if err != nil {
		return nil, fmt.Errorf("open descendant lock: %w", err)
	}
	if err := tryLock(file, false); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("hold descendant lock: %w", err)
	}
	return file, nil
}

// close unregisters the lock files before releasing the process lock. Callers
// release this instance's leases first, so a probe that now reports the
// instance as exited finds nothing to reclaim.
func (locks *instanceLocks) close() error {
	if locks == nil || locks.process == nil {
		return nil
	}
	removeInstanceFiles(locks.directory, locks.id)
	return locks.process.Close()
}

// probeInstance reports whether an instance or any descendant still holds
// its locks. Missing files mean the instance closed or was already reclaimed.
func probeInstance(directory, id string) (bool, error) {
	alive, err := probeLock(processLockPath(directory, id))
	if err != nil || alive {
		return alive, err
	}
	return probeLock(descendantLockPath(directory, id))
}

// probeLock tries an exclusive hold on a separate description. Locks belong
// to open file descriptions, so probing this process's own lock also reports
// it as held. Closing the probe file releases a successful probe.
func probeLock(path string) (bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	switch err := tryLock(file, true); {
	case err == nil:
		return false, nil
	case errors.Is(err, errLocked):
		return true, nil
	default:
		return false, err
	}
}

// removeInstanceFiles deletes the lock files of a closed or exited instance.
func removeInstanceFiles(directory, id string) {
	_ = os.Remove(processLockPath(directory, id))
	_ = os.Remove(descendantLockPath(directory, id))
}

// processLockPath is held exclusively by the instance process.
func processLockPath(directory, id string) string {
	return filepath.Join(directory, id+".lock")
}

// descendantLockPath is held shared by the instance's child processes.
func descendantLockPath(directory, id string) string {
	return filepath.Join(directory, id+".children.lock")
}

// newInstanceID returns a random identifier that is never reused.
func newInstanceID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate instance ID: %w", err)
	}
	return "instance_" + hex.EncodeToString(raw[:]), nil
}

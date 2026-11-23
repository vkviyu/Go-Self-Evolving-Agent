// Package runtimeowner enforces one live runtime per durable database. This is
// an OS-owned lifetime lock, not a PID file: process death releases ownership.
package runtimeowner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

var ErrOwned = errors.New("runtime database is already owned by another active instance")

type Lock struct {
	file *os.File
	once sync.Once
	err  error
}

// Acquire must run before opening or recovering the database. Canonical paths
// ensure a symlink alias cannot create a second ownership domain.
func Acquire(databasePath string) (*Lock, error) {
	if databasePath == "" || databasePath == ":memory:" {
		return nil, errors.New("runtime ownership requires a filesystem database path")
	}
	abs, err := filepath.Abs(databasePath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
		return nil, err
	}
	if canonical, err := filepath.EvalSymlinks(abs); err == nil {
		abs = canonical
	} else if errors.Is(err, os.ErrNotExist) {
		if info, inspectErr := os.Lstat(abs); inspectErr == nil && info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("database symlink target must exist before acquiring runtime ownership")
		}
		parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
		if err != nil {
			return nil, err
		}
		abs = filepath.Join(parent, filepath.Base(abs))
	} else {
		return nil, err
	}
	file, err := os.OpenFile(abs+".owner.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open runtime ownership lock: %w", err)
	}
	if err := tryLock(file); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("acquire runtime for %s: %w; close the other instance or use a separate data directory", abs, err)
	}
	return &Lock{file: file}, nil
}

func (lock *Lock) Close() error {
	if lock == nil {
		return nil
	}
	// Never unlink a lock file: replacing its inode permits two live owners.
	lock.once.Do(func() {
		// A concurrent fork can briefly retain this open file description
		// until exec closes inherited descriptors. Release ownership explicitly
		// so a completed Close does not depend on that child reaching exec.
		lock.err = errors.Join(unlock(lock.file), lock.file.Close())
	})
	return lock.err
}

var retained struct {
	sync.Mutex
	locks []*Lock
}

// RetainUntilExit prevents a timed-out worker from losing ownership while it
// might still mutate files. The OS closes the descriptor when this process exits.
func (lock *Lock) RetainUntilExit() {
	if lock == nil {
		return
	}
	retained.Lock()
	retained.locks = append(retained.locks, lock)
	retained.Unlock()
}

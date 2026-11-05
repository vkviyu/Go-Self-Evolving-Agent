package workspacedomain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const (
	rendezvousMutexName     = "rendezvous.mutex"
	rendezvousRecordVersion = 1
	rendezvousRecordLimit   = 64 << 10
	rendezvousMutexWait     = 5 * time.Second
)

type (
	// rendezvousRecord names the coordination home that currently owns a
	// workspace. It is only trusted while some member holds the lock file.
	rendezvousRecord struct {
		Version int    `json:"version"`
		Home    string `json:"home"`
		Root    string `json:"root"`
	}

	// membership is this process's shared hold on one workspace lock file.
	membership struct {
		directory string
		path      string
		file      *os.File
	}
)

// joinRendezvous admits home as a member of the workspace's coordination
// domain. Every join and leave runs under the directory-wide mutex: flock
// conversion is not atomic and leave unlinks the file, so the mutex keeps
// other processes out of both windows.
func joinRendezvous(ctx context.Context, directory string, identity Identity, home string) (*membership, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("%w: %v", errRendezvousUnavailable, err)
	}
	mutex, err := acquireRendezvousMutex(ctx, directory)
	if err != nil {
		return nil, err
	}
	defer mutex.Close()
	path := filepath.Join(directory, identity.ID+".lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open workspace rendezvous lock: %w", err)
	}
	joined := false
	defer func() {
		if !joined {
			_ = file.Close()
		}
	}()
	switch err := tryLock(file, true); {
	case err == nil:
		// No member holds the file, so any stored record is stale.
		if err := writeRendezvousRecord(file, rendezvousRecord{Version: rendezvousRecordVersion, Home: home, Root: identity.Root}); err != nil {
			return nil, err
		}
	case errors.Is(err, errLocked):
		record, err := readRendezvousRecord(file)
		if err != nil {
			return nil, err
		}
		if record.Home != home {
			return nil, fmt.Errorf("%w: %s is coordinated by GSEA_HOME %s; close those sessions or use the same GSEA_HOME", ErrForeignDomain, identity.Root, record.Home)
		}
	default:
		return nil, fmt.Errorf("lock workspace rendezvous: %w", err)
	}
	// Downgrading from exclusive or joining existing members both end with a
	// shared hold. Only mutex holders take the exclusive lock, so this cannot block.
	if err := tryLock(file, false); err != nil {
		return nil, fmt.Errorf("share workspace rendezvous lock: %w", err)
	}
	joined = true
	return &membership{directory: directory, path: path, file: file}, nil
}

// leave drops this member's hold and removes the lock file when no other
// member remains. If the mutex is unavailable, closing still releases the
// hold and the next first member overwrites the stale record.
func (member *membership) leave() error {
	if member == nil || member.file == nil {
		return nil
	}
	mutex, err := acquireRendezvousMutex(context.Background(), member.directory)
	if err != nil {
		return errors.Join(err, member.file.Close())
	}
	defer mutex.Close()
	var errs []error
	if err := unlock(member.file); err != nil {
		errs = append(errs, fmt.Errorf("unlock workspace rendezvous: %w", err))
	}
	if tryLock(member.file, true) == nil {
		if err := os.Remove(member.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove workspace rendezvous lock: %w", err))
		}
	}
	errs = append(errs, member.file.Close())
	return errors.Join(errs...)
}

// acquireRendezvousMutex holds the directory-wide mutex. The mutex file is
// never unlinked; replacing its inode would admit two holders.
func acquireRendezvousMutex(ctx context.Context, directory string) (*os.File, error) {
	file, err := os.OpenFile(filepath.Join(directory, rendezvousMutexName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errRendezvousUnavailable, err)
	}
	if err := holdMutex(ctx, file, rendezvousMutexWait); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("rendezvous mutex in %s: %w", directory, err)
	}
	return file, nil
}

// writeRendezvousRecord replaces the record and syncs it before any other
// process can observe the shared hold that makes it trusted.
func writeRendezvousRecord(file *os.File, record rendezvousRecord) error {
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("reset workspace rendezvous record: %w", err)
	}
	if _, err := file.WriteAt(encoded, 0); err != nil {
		return fmt.Errorf("write workspace rendezvous record: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync workspace rendezvous record: %w", err)
	}
	return nil
}

// readRendezvousRecord parses the record of a held lock file. A held file
// always has a complete record, so a parse failure is reported, not ignored.
func readRendezvousRecord(file *os.File) (rendezvousRecord, error) {
	data, err := io.ReadAll(io.NewSectionReader(file, 0, rendezvousRecordLimit))
	if err != nil {
		return rendezvousRecord{}, fmt.Errorf("read workspace rendezvous record: %w", err)
	}
	var record rendezvousRecord
	if err := json.Unmarshal(data, &record); err != nil || record.Version != rendezvousRecordVersion || record.Home == "" {
		return rendezvousRecord{}, fmt.Errorf("workspace rendezvous record %s is unreadable", file.Name())
	}
	return record, nil
}

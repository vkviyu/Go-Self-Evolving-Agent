package tool

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

var ErrSnapshotUnavailable = errors.New("verified registry snapshot is unavailable")
var ErrSnapshotHostChanged = fmt.Errorf("%w: host executable changed", ErrSnapshotUnavailable)

// Native implementation identity includes the host executable, not its path or
// a process-local counter. A rebuilt host must not silently resume old code.
// The binding identity also captures execution configuration within that host.
var hostIdentity = sync.OnceValues(func() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
})

// ID is a content address, stable across registry publication order/processes.
// Revision remains a semantic contract: changing executor configuration must
// change its revision or ImplementationID even within the same host binary.
func (snapshot *Snapshot) ID() (string, error) {
	if snapshot == nil {
		return "", ErrSnapshotUnavailable
	}
	snapshot.identityOnce.Do(func() {
		host, err := hostIdentity()
		if err != nil {
			snapshot.identityErr = fmt.Errorf("identify runtime executable: %w", err)
			return
		}
		manifest := SnapshotManifest{Version: 1, Host: host}
		for _, descriptor := range snapshot.Descriptors() {
			manifest.Tools = append(manifest.Tools, SnapshotEntry{descriptor, snapshot.bindings[descriptor.Name].ImplementationID})
		}
		snapshot.identity, snapshot.identityErr = manifest.ID()
	})
	return snapshot.identity, snapshot.identityErr
}

// SnapshotByID looks up an already owned view without I/O. ResolveSnapshot is
// the explicit durable recovery path. Legacy registry-N IDs remain rejected.
func (registry *Registry) SnapshotByID(identity string) (*Snapshot, error) {
	if !strings.HasPrefix(identity, "registry-sha256:") {
		return nil, fmt.Errorf("%w: legacy or invalid identity %q; start a new turn after inspecting prior effects", ErrSnapshotUnavailable, identity)
	}
	registry.mu.Lock()
	snapshots := make([]*Snapshot, 0, len(registry.snapshots))
	for _, snapshot := range registry.snapshots {
		snapshots = append(snapshots, snapshot)
	}
	registry.mu.Unlock()
	// Identity construction may hash the executable and serialize manifests;
	// do not block concurrent publication while performing that work.
	for _, snapshot := range snapshots {
		id, err := snapshot.ID()
		if err != nil {
			return nil, err
		}
		if id == identity {
			return snapshot, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrSnapshotUnavailable, identity)
}

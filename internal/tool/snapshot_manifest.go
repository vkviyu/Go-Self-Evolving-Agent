package tool

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"
)

const MaxSnapshotManifestBytes = 1 << 20

var ErrSnapshotReferenceConflict = errors.New("snapshot reference cannot be rebound")
var ErrSnapshotReferenceReleased = errors.New("snapshot reference was released; recovery cannot reopen it")

// SnapshotManifest uses the original v1 content-address format. It describes
// exact implementations, never serializes executors, and grants no authority.
type SnapshotManifest struct {
	Version int
	Host    string
	Tools   []SnapshotEntry
}

type SnapshotEntry struct {
	Descriptor     Descriptor
	Implementation string
}

func canonicalEntry(entry SnapshotEntry) (SnapshotEntry, error) {
	if err := entry.Descriptor.Validate(); err != nil {
		return SnapshotEntry{}, err
	}
	entry.Descriptor = cloneDescriptor(entry.Descriptor)
	decoder := json.NewDecoder(bytes.NewReader(entry.Descriptor.InputSchema))
	decoder.UseNumber()
	var schema any
	if err := decoder.Decode(&schema); err != nil {
		return SnapshotEntry{}, err
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		return SnapshotEntry{}, err
	}
	entry.Descriptor.InputSchema = encoded
	sort.Strings(entry.Descriptor.Capabilities)
	return entry, nil
}

// Matches compares executable identity and all canonical model/policy metadata.
func (entry SnapshotEntry) Matches(binding Binding) bool {
	left, err := canonicalEntry(entry)
	if err != nil {
		return false
	}
	right, err := canonicalEntry(SnapshotEntry{binding.Descriptor, binding.ImplementationID})
	if err != nil {
		return false
	}
	a, _ := json.Marshal(left)
	b, _ := json.Marshal(right)
	return bytes.Equal(a, b)
}

// Encode validates the manifest and serializes its entries in tool-name order for stable
// snapshot IDs.
func (manifest SnapshotManifest) Encode() ([]byte, error) {
	host, err := hex.DecodeString(manifest.Host)
	if manifest.Version != 1 || err != nil || len(host) != sha256.Size || hex.EncodeToString(host) != manifest.Host || len(manifest.Tools) > 1024 {
		return nil, fmt.Errorf("%w: invalid manifest version, host or tool count", ErrSnapshotUnavailable)
	}
	var entries []SnapshotEntry
	for _, entry := range manifest.Tools {
		canonical, err := canonicalEntry(entry)
		if err != nil {
			return nil, err
		}
		entries = append(entries, canonical)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Descriptor.Name < entries[j].Descriptor.Name })
	for i := 1; i < len(entries); i++ {
		if entries[i-1].Descriptor.Name == entries[i].Descriptor.Name {
			return nil, fmt.Errorf("%w: duplicate snapshot tool", ErrSnapshotUnavailable)
		}
	}
	manifest.Tools = entries
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	if len(encoded) > MaxSnapshotManifestBytes {
		return nil, fmt.Errorf("%w: snapshot manifest exceeds 1 MiB", ErrSnapshotUnavailable)
	}
	return encoded, nil
}

// ID hashes the canonical manifest bytes and returns a registry-sha256 identifier.
func (manifest SnapshotManifest) ID() (string, error) {
	encoded, err := manifest.Encode()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return "registry-sha256:" + hex.EncodeToString(digest[:]), nil
}

func DecodeSnapshotManifest(encoded []byte) (SnapshotManifest, error) {
	var manifest SnapshotManifest
	if len(encoded) > MaxSnapshotManifestBytes {
		return manifest, ErrSnapshotUnavailable
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return manifest, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return manifest, ErrSnapshotUnavailable
	}
	_, err := manifest.Encode()
	return manifest, err
}

// Manifest builds the host-bound manifest describing the snapshot's tool bindings.
func (snapshot *Snapshot) Manifest() (SnapshotManifest, error) {
	if snapshot == nil {
		return SnapshotManifest{}, ErrSnapshotUnavailable
	}
	host, err := hostIdentity()
	if err != nil {
		return SnapshotManifest{}, err
	}
	manifest := SnapshotManifest{Version: 1, Host: host}
	for _, descriptor := range snapshot.Descriptors() {
		entry, err := canonicalEntry(SnapshotEntry{descriptor, snapshot.bindings[descriptor.Name].ImplementationID})
		if err != nil {
			return SnapshotManifest{}, err
		}
		manifest.Tools = append(manifest.Tools, entry)
	}
	return manifest, nil
}

// References are immutable retention roots, not evidence that work is active.
// Turn: Scope=session, ID=turn. Flow: Scope=team, ID=idempotency key.
// Release is an offline consumer-lifecycle decision, never a generic GC call.
type SnapshotReference struct{ Kind, Scope, ID string }

type SnapshotReferenceRecord struct {
	Reference     SnapshotReference
	ReleasedAt    *time.Time
	ReleaseReason string
}

// Validate checks that the reference names a turn or flow and has a bounded, nonempty
// scope and ID.
func (reference SnapshotReference) Validate() error {
	if (reference.Kind != "turn" && reference.Kind != "flow") || reference.Scope == "" || reference.ID == "" || len(reference.Scope) > 4096 || len(reference.ID) > 4096 {
		return errors.New("invalid runtime snapshot reference")
	}
	return nil
}

type SnapshotStore interface {
	Save(context.Context, SnapshotManifest, SnapshotReference) error
	Load(context.Context, string) (SnapshotManifest, error)
}

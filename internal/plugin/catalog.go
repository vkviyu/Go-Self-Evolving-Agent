package plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	CatalogVersion   = 1
	MaxArtifactBytes = 32 << 20
	MaxManifestBytes = 64 << 10
	// Includes repeated references, matching the startup loader's memory use.
	MaxCatalogBytes = 128 << 20
)

var (
	ErrCatalogConflict      = errors.New("plugin catalog conflict")
	ErrCatalogInvalid       = errors.New("invalid plugin catalog record")
	ErrPersistenceUncertain = errors.New("plugin persistence failed; reopen the runtime before further mutations")
	ErrToolOwned            = errors.New("plugin cannot replace another owner's tool")
)

// RevisionRecord is immutable. State is a separate transactional projection,
// so a lifecycle change never rewrites the artifact's manifest or content.
type RevisionRecord struct {
	Version         int
	Manifest        Manifest
	ContentSHA256   string
	RuntimeIdentity string
}

type StoredArtifact struct {
	Record RevisionRecord
	Source []byte
	State  State
}

// Catalog is owned by one runtime (the App holds the database ownership lock).
// Save atomically stores the blob and manifest as staged. SetActive atomically
// retires the previous revision and promotes the new one; empty next retires
// without promoting. expected is the exact previously active revision.
type Catalog interface {
	Save(context.Context, RevisionRecord, []byte) error
	Load(context.Context) ([]StoredArtifact, error)
	SetActive(ctx context.Context, toolName, expected, next string) error
}

func (record RevisionRecord) Encode() ([]byte, error) {
	if record.Version != CatalogVersion || record.RuntimeIdentity == "" || len(record.RuntimeIdentity) > 512 {
		return nil, ErrCatalogInvalid
	}
	manifest := record.Manifest
	if manifest.PluginID == "" || len(manifest.PluginID) > 128 || len(manifest.Tool.Revision) > 128 || manifest.Timeout <= 0 {
		return nil, ErrCatalogInvalid
	}
	if err := manifest.Tool.Validate(); err != nil {
		return nil, err
	}
	digest, err := hex.DecodeString(record.ContentSHA256)
	if err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != record.ContentSHA256 {
		return nil, ErrCatalogInvalid
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	if len(encoded) > MaxManifestBytes {
		return nil, fmt.Errorf("%w: manifest too large", ErrCatalogInvalid)
	}
	return encoded, nil
}

func DecodeRecord(encoded []byte) (RevisionRecord, error) {
	var record RevisionRecord
	if len(encoded) > MaxManifestBytes {
		return record, ErrCatalogInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return record, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return record, ErrCatalogInvalid
	}
	_, err := record.Encode()
	return record, err
}

func (record RevisionRecord) ValidateSource(source []byte) error {
	if _, err := record.Encode(); err != nil {
		return err
	}
	if len(source) == 0 || len(source) > MaxArtifactBytes {
		return fmt.Errorf("%w: invalid artifact size", ErrCatalogInvalid)
	}
	sum := sha256.Sum256(source)
	if hex.EncodeToString(sum[:]) != record.ContentSHA256 {
		return fmt.Errorf("%w: WASM checksum mismatch", ErrCatalogInvalid)
	}
	return nil
}

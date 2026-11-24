// Package evolutionstore implements strategy evidence ports using individual
// ATE authority records. It owns no local business database or cached projection.
package evolutionstore

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/retention"
	"github.com/vkviyu/go-self-evolving-agent/internal/strategy"
)

type Store struct{ authority *ateauthority.Store }

var (
	_ strategy.Catalog     = (*Store)(nil)
	_ strategy.ShadowStore = (*Store)(nil)
)

func New(authority *ateauthority.Store) (*Store, error) {
	if authority == nil {
		return nil, errors.New("evolution store requires ATE authority")
	}
	return &Store{authority: authority}, nil
}

const prefix = "gsea/evolution/v1/"

func recordKey(kind string, ids ...string) string {
	parts := []string{prefix + kind}
	for _, id := range ids {
		parts = append(parts, base64.RawURLEncoding.EncodeToString([]byte(id)))
	}
	return strings.Join(parts, "/")
}

// Keep the exact encoded bytes: checksums must not silently accept a decoder's
// normalization of a corrupted record. Each envelope contains only one entity.
type document struct {
	Data   []byte `json:"data"`
	Digest string `json:"digest"`
}

func encode(value any) (document, error) {
	raw, err := json.Marshal(value)
	return document{Data: raw, Digest: retention.Digest(raw)}, err
}

func missing(kind, id string) error {
	return fmt.Errorf("%w: %s %s", orchestration.ErrNotFound, kind, id)
}

func loadDocument(tx *ateauthority.Tx, kind, id string, out any) error {
	var doc document
	found, err := tx.Get(recordKey(kind, id), &doc)
	if err != nil {
		return err
	}
	if !found {
		return missing(kind, id)
	}
	if doc.Digest != id || retention.Digest(doc.Data) != id {
		return fmt.Errorf("%s checksum mismatch", kind)
	}
	return json.Unmarshal(doc.Data, out)
}

func saveDocument(tx *ateauthority.Tx, kind string, doc document) error {
	key := recordKey(kind, doc.Digest)
	var stored document
	found, err := tx.Get(key, &stored)
	if err != nil {
		return err
	}
	if found {
		if stored.Digest != doc.Digest || !bytes.Equal(stored.Data, doc.Data) {
			return fmt.Errorf("%s integrity mismatch", kind)
		}
		return nil
	}
	return tx.Create(key, doc)
}

// Preserve fatal evidence classification when a publication commit is rejected
// or uncertain, without making ordinary validation errors fatal.
func publicationError(err error) error {
	var requestErr *ateauthority.RequestError
	if errors.As(err, &requestErr) {
		return errors.Join(strategy.ErrEvidence, err)
	}
	return err
}

package testfixture

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateports"
)

// State stores exact decision bytes in the fixture's native authority.
type State struct{ Authority *ateauthority.Store }

type stateValue struct{ Bytes []byte }

// stateKey encodes a fixture state key under the isolated test experience namespace.
func stateKey(key string) string {
	return "test/experience/" + base64.RawURLEncoding.EncodeToString([]byte(key))
}

func (s State) Get(ctx context.Context, key string) (json.RawMessage, bool, error) {
	var value stateValue
	var found bool
	err := s.Authority.View(ctx, func(tx *ateauthority.Tx) error {
		var err error
		found, err = tx.Get(stateKey(key), &value)
		return err
	})
	return value.Bytes, found, err
}

func (s State) Put(ctx context.Context, key string, value json.RawMessage) error {
	return s.Authority.Update(ctx, func(tx *ateauthority.Tx) error {
		var prior stateValue
		found, err := tx.Get(stateKey(key), &prior)
		if err != nil {
			return err
		}
		if found {
			if !bytes.Equal(prior.Bytes, value) {
				return ateports.ErrConflict
			}
			return nil
		}
		return tx.Create(stateKey(key), stateValue{Bytes: bytes.Clone(value)})
	})
}

func (s State) CompareAndSwap(ctx context.Context, key string, expected, value json.RawMessage) error {
	return s.Authority.Update(ctx, func(tx *ateauthority.Tx) error {
		var prior stateValue
		found, err := tx.Get(stateKey(key), &prior)
		if err != nil {
			return err
		}
		if !found || !bytes.Equal(prior.Bytes, expected) {
			return ateports.ErrConflict
		}
		return tx.Put(stateKey(key), stateValue{Bytes: bytes.Clone(value)})
	})
}

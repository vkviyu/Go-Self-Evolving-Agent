package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateports"
)

type (
	// authorityState keeps semantic decisions in ATE alongside their sources.
	// It is not a local history cache or a second event delivery queue.
	authorityState struct {
		records *ateauthority.Store
	}
	authorityStateValue struct {
		// Bytes preserves the exact JSON spelling required by the state CAS port.
		Bytes []byte
	}
)

// authorityStateKey keeps arbitrary caller keys inside a distinct namespace.
func authorityStateKey(key string) string {
	return "gsea/experience/v1/" + base64.RawURLEncoding.EncodeToString([]byte(key))
}

// Get reads one value from an immutable ATE view.
func (s authorityState) Get(ctx context.Context, key string) (json.RawMessage, bool, error) {
	if key == "" {
		return nil, false, ateports.ErrInvalid
	}
	var value authorityStateValue
	var found bool
	err := s.records.View(ctx, func(tx *ateauthority.Tx) error {
		var err error
		found, err = tx.Get(authorityStateKey(key), &value)
		return err
	})
	return json.RawMessage(value.Bytes), found, err
}

// Put inserts once and permits byte-identical retries.
func (s authorityState) Put(ctx context.Context, key string, value json.RawMessage) error {
	if key == "" || !json.Valid(value) {
		return ateports.ErrInvalid
	}
	return s.records.Update(ctx, func(tx *ateauthority.Tx) error {
		var current authorityStateValue
		found, err := tx.Get(authorityStateKey(key), &current)
		if err != nil {
			return err
		}
		if found {
			if !bytes.Equal(current.Bytes, value) {
				return fmt.Errorf("%w: experience state already exists", ateports.ErrConflict)
			}
			return nil
		}
		return tx.Create(authorityStateKey(key), authorityStateValue{Bytes: bytes.Clone(value)})
	})
}

// CompareAndSwap protects semantic work ownership with the authority's CAS.
func (s authorityState) CompareAndSwap(ctx context.Context, key string, expected, value json.RawMessage) error {
	if key == "" || !json.Valid(expected) || !json.Valid(value) {
		return ateports.ErrInvalid
	}
	return s.records.Update(ctx, func(tx *ateauthority.Tx) error {
		var current authorityStateValue
		found, err := tx.Get(authorityStateKey(key), &current)
		if err != nil {
			return err
		}
		if !found || !bytes.Equal(current.Bytes, expected) {
			return ateports.ErrConflict
		}
		return tx.Put(authorityStateKey(key), authorityStateValue{Bytes: bytes.Clone(value)})
	})
}

var (
	_ ateports.StateStore          = authorityState{}
	_ ateports.CompareAndSwapStore = authorityState{}
)

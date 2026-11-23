package ateauthority

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/model"
)

// Legacy scan constants remain source-compatible. List now uses native pages;
// these constants no longer impose a whole-space or total-result ceiling.
const (
	MaxListEntries   = 4096
	MaxListNodes     = 32768
	MaxListKeyBytes  = 4 << 20
	MaxListReadBytes = 16 << 20
	MaxListJSONBytes = 16 << 20
	// MaxListPages bounds total work for one callback enumeration. Each native
	// page has at most api.MaxStatePageItems and api.MaxStatePageBytes.
	MaxListPages = 65536
)

// Tx is valid only during its callback, on that callback's goroutine. The only
// local state is the bounded, uncommitted write set. Repeated reads go to ATE at
// the same anchor unless shadowed by a staged write.
type Tx struct {
	ctx      context.Context
	client   *client.Client
	view     api.ViewDescriptor
	root     model.ObjectID
	rootSet  model.ObjectID
	writable bool
	fenceAll bool
	active   bool
	writes   map[string]api.StateMutation
}

func (tx *Tx) Context() context.Context { return tx.ctx }

func (tx *Tx) check(write bool) error {
	if !tx.active {
		return ErrClosed
	}
	if err := tx.ctx.Err(); err != nil {
		return err
	}
	if write && !tx.writable {
		return ErrReadOnly
	}
	return nil
}

func validKey(key string) error {
	if len(key) > api.MaxStateKeyBytes {
		return fmt.Errorf("%w: key exceeds %d bytes", ErrLimit, api.MaxStateKeyBytes)
	}
	return nil
}

// Get decodes one JSON entity; absence leaves out untouched. out must be a JSON
// decoding destination. JSON null is a present value, distinct from absence.
func (tx *Tx) Get(key string, out any) (bool, error) {
	if err := tx.check(false); err != nil {
		return false, err
	}
	if err := validKey(key); err != nil {
		return false, err
	}
	raw, found, err := tx.raw(key, tx.writes)
	if err != nil || !found {
		return found, err
	}
	return true, json.Unmarshal(raw, out)
}

func (tx *Tx) raw(key string, writes map[string]api.StateMutation) (json.RawMessage, bool, error) {
	if op, ok := writes[key]; ok {
		if op.Kind == api.MutationDelete {
			return nil, false, nil
		}
		return bytes.Clone(op.Value.Data), true, nil
	}
	result, err := tx.client.GetViewState(tx.ctx, api.ReadViewStateRequest{View: tx.view.Ref, Key: []byte(key)})
	if err != nil {
		return nil, false, mapReadError(err)
	}
	if result.Fence != tx.view.Anchor.Fence || result.StateRoot != tx.root || result.RootSet != tx.rootSet {
		return nil, false, ErrIdentity
	}
	if !result.Found {
		return nil, false, nil
	}
	if result.Value.Kind != api.ValueBytes || !json.Valid(result.Value.Data) {
		return nil, true, ErrCorrupt
	}
	if len(result.Value.Data) > api.MaxStateValueBytes {
		return nil, true, ErrLimit
	}
	return bytes.Clone(result.Value.Data), true, nil
}

func (tx *Tx) Put(key string, value any) error {
	if err := tx.check(true); err != nil {
		return err
	}
	if err := validKey(key); err != nil {
		return err
	}
	raw, err := marshalValue(value)
	if err != nil {
		return err
	}
	if len(raw) > api.MaxStateValueBytes {
		return ErrLimit
	}
	return tx.stage(key, api.StateMutation{Key: []byte(key), Kind: api.MutationPut,
		Value: api.Value{Kind: api.ValueBytes, Data: raw}})
}

// Create checks absence in the transaction overlay and anchor. ExpectedRoot
// protects the absence predicate from concurrent creation before commit.
func (tx *Tx) Create(key string, value any) error {
	if err := tx.check(true); err != nil {
		return err
	}
	if err := validKey(key); err != nil {
		return err
	}
	_, found, err := tx.raw(key, tx.writes)
	if err != nil {
		return err
	}
	if found {
		return ErrExists
	}
	return tx.Put(key, value)
}

// Delete is idempotent for an absent key.
func (tx *Tx) Delete(key string) error {
	if err := tx.check(true); err != nil {
		return err
	}
	if err := validKey(key); err != nil {
		return err
	}
	return tx.stage(key, api.StateMutation{Key: []byte(key), Kind: api.MutationDelete})
}

func (tx *Tx) stage(key string, op api.StateMutation) error {
	// Encode before replacing the overlay so an invalid/oversize operation
	// leaves all earlier staged values intact, even if its error is handled.
	ops := make([]api.StateMutation, 0, len(tx.writes)+1)
	for k, existing := range tx.writes {
		if k != key {
			ops = append(ops, existing)
		}
	}
	ops = append(ops, op)
	if len(ops) > api.MaxStateMutations {
		return ErrLimit
	}
	if _, err := api.EncodeStateBatch(api.StateBatch{ExpectedRoot: &tx.root, Operations: ops}); err != nil {
		return errors.Join(ErrLimit, err)
	}
	tx.writes[key] = op
	return nil
}

// List visits matching keys in bytewise lexical order, including staged writes
// and excluding staged deletes. It snapshots the overlay at List entry; writes
// from visit affect subsequent operations, not this enumeration. Raw messages
// are owned by the visitor. No enumeration is retained after the call.
//
// Native pages seek by prefix and exclusive key on the same pinned view. Only
// one bounded page and the bounded staged-write set are retained. Visitors may
// have run before an error; propagate errors and keep side effects out of visit.
func (tx *Tx) List(prefix string, visit func(key string, raw json.RawMessage) error) error {
	if err := tx.check(false); err != nil {
		return err
	}
	if visit == nil {
		return ErrInvalid
	}
	if err := validKey(prefix); err != nil {
		return err
	}
	writes := maps.Clone(tx.writes)
	var staged []string
	for key := range writes {
		if strings.HasPrefix(key, prefix) {
			staged = append(staged, key)
		}
	}
	slices.Sort(staged)
	emit := func(key string, value api.Value) error {
		if err := tx.check(false); err != nil {
			return err
		}
		if value.Kind != api.ValueBytes || !json.Valid(value.Data) {
			return ErrCorrupt
		}
		return visit(key, bytes.Clone(value.Data))
	}
	emitStaged := func(key string) error {
		op := writes[key]
		if op.Kind == api.MutationDelete {
			return nil
		}
		return emit(key, op.Value)
	}
	index := 0
	var continuation string
	var previous []byte
	for count := 0; count < MaxListPages; count++ {
		page, err := tx.client.GetViewStatePage(tx.ctx, api.ReadViewStatePageRequest{
			View: tx.view.Ref, Prefix: []byte(prefix), Continuation: continuation,
			Limit: api.MaxStatePageItems, MaxBytes: api.MaxStatePageBytes,
		})
		if err != nil {
			return mapReadError(err)
		}
		if page.Anchor.Space != tx.view.Anchor.Space || page.Anchor.Fence != tx.view.Anchor.Fence ||
			page.Anchor.Cursor != nil || page.Anchor.Knowledge != nil ||
			page.Fence != tx.view.Anchor.Fence || page.StateRoot != tx.root || page.RootSet != tx.rootSet {
			return ErrIdentity
		}
		for _, item := range page.Items {
			if previous != nil && bytes.Compare(previous, item.Key) >= 0 {
				return ErrIdentity
			}
			previous = bytes.Clone(item.Key)
			if previous == nil {
				previous = []byte{}
			}
			key := string(item.Key)
			for index < len(staged) && staged[index] < key {
				if err := emitStaged(staged[index]); err != nil {
					return err
				}
				index++
			}
			if index < len(staged) && staged[index] == key {
				if err := emitStaged(staged[index]); err != nil {
					return err
				}
				index++
			} else if err := emit(key, item.Value); err != nil {
				return err
			}
		}
		if page.Continuation == "" {
			for ; index < len(staged); index++ {
				if err := emitStaged(staged[index]); err != nil {
					return err
				}
			}
			return nil
		}
		if page.Continuation == continuation {
			return ErrIdentity
		}
		continuation = page.Continuation
	}
	return ErrLimit
}

// marshalValue writes canonical JSON without HTML escaping. Escaping "<", ">"
// and "&" would inflate a bounded text value past the native byte limit.
func marshalValue(value any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

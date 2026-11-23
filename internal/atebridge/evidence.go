package atebridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/codec"
	"github.com/vkviyu/agent-temporal-engine/memory"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateports"
	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
)

// RecoverPending reconciles a bounded pass of this bridge's original commands.
// It never reads source events. Other owners sharing Pending are left alone.
func (b *Bridge) RecoverPending(ctx context.Context) error {
	return b.withMutation(ctx, func(c *client.Client) error {
		snapshot, err := c.Snapshot(ctx)
		if err != nil {
			return err
		}
		executor, err := aterequest.NewExecutor(c, b.pending, snapshot.Fence.Store)
		if err != nil {
			return err
		}
		options := aterequest.ListOptions{Limit: aterequest.MaxListRecords, MaxBytes: aterequest.MaxListBytes}
		for scanned := 0; scanned < 10_000; {
			page, err := b.pending.List(ctx, options)
			if err != nil {
				return err
			}
			if len(page) == 0 {
				return nil
			}
			for _, record := range page {
				scanned++
				key := record.Request.Key()
				if key.Principal == "gsea" && strings.HasPrefix(string(key.RequestID), b.requestPrefix()) {
					if _, err := executor.Resume(ctx, key); err != nil {
						return nativeRequestError(err)
					}
				}
				options.After = &key
			}
		}
		return ErrBacklog
	})
}

// putValue authors execution goals, outcomes and validation records only.
// Source history remains in the original native session nodes.
func (b *Bridge) putValue(ctx context.Context, c *client.Client, key string, data []byte) (memory.Source, error) {
	id, err := valueID(b.OperationalSpace(), data)
	if err != nil {
		return memory.Source{}, err
	}
	stateKey := b.valueKey(key)
	result, err := b.execute(ctx, c, "value/"+key, func(*client.Client) (api.CommitRequest, error) {
		raw, err := api.EncodeStateBatch(api.StateBatch{Operations: []api.StateMutation{{
			Key: stateKey, Kind: api.MutationPut, Value: api.Value{Kind: api.ValueBytes, Data: data},
			Condition: api.ValueCondition{Kind: api.ConditionAbsent},
		}}})
		return api.CommitRequest{Space: b.OperationalSpace(), Method: api.MethodMutateState, Body: raw}, err
	})
	if err != nil {
		return memory.Source{}, err
	}
	source := memory.Source{SourceID: id, Anchor: model.SnapshotAnchor{Space: b.OperationalSpace(), Fence: result.Fence}}
	// Read at the original publication under current policy on every return.
	view, err := c.OpenView(ctx, api.OpenViewRequest{Space: source.Anchor.Space, Principal: "gsea", Anchor: &source.Anchor})
	if err != nil {
		return memory.Source{}, err
	}
	value, readErr := c.GetViewState(ctx, api.ReadViewStateRequest{View: view.Ref, Key: stateKey})
	closeErr := c.CloseView(ctx, api.CloseViewRequest{Ref: view.Ref})
	if err := errors.Join(readErr, closeErr); err != nil {
		return memory.Source{}, err
	}
	if !value.Found || value.Value.Kind != api.ValueBytes || !bytes.Equal(value.Value.Data, data) {
		return memory.Source{}, fmt.Errorf("atebridge: immutable value differs from original: %w", ateports.ErrConflict)
	}
	return source, nil
}

func (b *Bridge) valueKey(key string) []byte {
	return []byte("gsea/execution-values/v1/" + digest(b.namespace, key))
}

// ATE currently exports its object codec but not makeValue. This is the frozen
// ate.value/v1 byte-value encoding, tested against native state entry identities.
func valueID(space model.SpaceID, data []byte) (model.ObjectID, error) {
	if len(data) > api.MaxStateValueBytes {
		return "", errors.New("atebridge: value exceeds 1 MiB")
	}
	payload, err := codec.AppendBytes([]byte{1, 0, 1}, data)
	if err != nil {
		return "", err
	}
	return codec.ObjectID(codec.Object{Type: "ate.value", Space: space, Version: 1, Payload: payload})
}

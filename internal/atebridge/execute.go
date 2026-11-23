package atebridge

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
)

func (b *Bridge) execute(ctx context.Context, c *client.Client, key string, build func(*client.Client) (api.CommitRequest, error)) (api.CommitResult, error) {
	return b.executeNative(ctx, c, b.OperationalSpace(), key, build)
}

func (b *Bridge) requestID(key string) model.RequestID {
	if key == "namespace-binding" {
		return model.RequestID("gsea-native-config-" + digest(b.namespace))
	}
	return model.RequestID(b.requestPrefix() + digest(key))
}

func (b *Bridge) requestPrefix() string {
	return "gsea-native-" + digest(b.namespace, string(b.OperationalSpace())) + "-"
}

// executeNative first reconciles the deterministic original key. Only proven
// absence with no pending envelope permits building new bytes. Acknowledged
// commands live solely in ATE; no permanent local request/receipt cache exists.
func (b *Bridge) executeNative(ctx context.Context, c *client.Client, space model.SpaceID, operation string, build func(*client.Client) (api.CommitRequest, error)) (api.CommitResult, error) {
	if strings.TrimSpace(strings.TrimPrefix(operation, "caller/")) == "" || build == nil {
		return api.CommitResult{}, errors.New("atebridge: nonempty operation key and builder required")
	}
	key := api.RequestKey{Space: space, Principal: "gsea", RequestID: b.requestID(operation)}
	if err := key.Validate(); err != nil {
		return api.CommitResult{}, err
	}
	snapshot, err := c.Snapshot(ctx)
	if err != nil {
		return api.CommitResult{}, err
	}
	executor, err := aterequest.NewExecutor(c, b.pending, snapshot.Fence.Store)
	if err != nil {
		return api.CommitResult{}, err
	}
	_, pending, err := b.pending.Get(ctx, key)
	if err != nil {
		return api.CommitResult{}, err
	}
	if pending {
		result, err := executor.Resume(ctx, key)
		return result, nativeRequestError(err)
	}
	lookup, err := c.LookupRequest(ctx, api.LookupRequestRequest{Space: space, Principal: key.Principal, RequestID: key.RequestID})
	if err != nil {
		return api.CommitResult{}, errors.Join(ErrOutcomeUnknown, err)
	}
	if err := verifyNativeLookup(key, snapshot.Fence.Store, lookup); err != nil {
		return api.CommitResult{}, err
	}
	if lookup.Code == api.CodeDurableAck {
		return lookup.Result, nil
	}
	request, err := build(c)
	if err != nil {
		return api.CommitResult{}, err
	}
	if request.Space != space {
		return api.CommitResult{}, errors.New("atebridge: native request space differs from explicit target")
	}
	request.Principal, request.RequestID = key.Principal, key.RequestID
	result, err := executor.Execute(ctx, request)
	return result, nativeRequestError(err)
}

func verifyNativeLookup(key api.RequestKey, store model.StoreID, lookup api.LookupRequestResult) error {
	if lookup.Code != api.CodeDurableAck && lookup.Code != api.CodeNotFoundAtFence {
		return fmt.Errorf("%w: lookup %s for %s", ErrOutcomeUnknown, lookup.Code, key.RequestID)
	}
	if lookup.ObservedFence.Store != store || lookup.ObservedFence.Validate() != nil {
		return errors.Join(ErrStoreChanged, aterequest.ErrConflict)
	}
	if lookup.Code == api.CodeDurableAck {
		result := lookup.Result
		if result.Code != api.CodeDurableAck || result.RequestID != key.RequestID ||
			!model.ValidDigest(string(result.RequestDigest)) || result.Fence.Store != store ||
			result.Fence.Validate() != nil || result.Fence.LSN == 0 ||
			lookup.ObservedFence.LSN < result.Fence.LSN ||
			(lookup.ObservedFence.LSN == result.Fence.LSN && lookup.ObservedFence != result.Fence) {
			return aterequest.ErrConflict
		}
	}
	return nil
}

func nativeRequestError(err error) error {
	if errors.Is(err, aterequest.ErrUnknown) {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	return err
}

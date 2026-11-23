package aterequest

import (
	"encoding/json"
	"fmt"

	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/model"
)

// Rejection permanently reserves a terminal key without retaining its body.
// Retired denotes a local decision never to submit, and requires a zero Result;
// it must never be interpreted as a native rejection or acknowledgement.
type Rejection struct {
	StoreID model.StoreID
	Key     api.RequestKey
	Digest  model.ObjectID
	Result  api.CommitResult
	Retired bool
}

func (rejection Rejection) Validate() error {
	if !validStoreID(rejection.StoreID) || !model.ValidDigest(string(rejection.Digest)) {
		return fmt.Errorf("%w: terminal identity", ErrInvalid)
	}
	if err := rejection.Key.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if rejection.Retired {
		if rejection.Result != (api.CommitResult{}) {
			return fmt.Errorf("%w: retirement cannot contain a native result", ErrInvalid)
		}
		return nil
	}
	return verifyRejection(rejection.Key, rejection.Result)
}

type rejectionWire struct {
	Version int              `json:"version"`
	StoreID model.StoreID    `json:"store_id"`
	Key     api.RequestKey   `json:"key"`
	Digest  model.ObjectID   `json:"digest"`
	Result  api.CommitResult `json:"result"`
	Retired bool             `json:"retired"`
}

func EncodeRejection(rejection Rejection) ([]byte, error) {
	if err := rejection.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(rejectionWire{
		Version: 1, StoreID: rejection.StoreID, Key: rejection.Key,
		Digest: rejection.Digest, Result: rejection.Result, Retired: rejection.Retired,
	})
}

func DecodeRejection(raw []byte) (Rejection, error) {
	var wire rejectionWire
	if err := decodeCanonical(raw, 64<<10, &wire); err != nil {
		return Rejection{}, err
	}
	if wire.Version != 1 {
		return Rejection{}, fmt.Errorf("%w: unsupported terminal version", ErrCorrupt)
	}
	rejection := Rejection{StoreID: wire.StoreID, Key: wire.Key, Digest: wire.Digest, Result: wire.Result, Retired: wire.Retired}
	if err := rejection.Validate(); err != nil {
		return Rejection{}, fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
	return rejection, nil
}

// VerifyRejection checks the conclusive native response shape. Only the
// persistence adapter can additionally prove a durable attempt and ensure the
// response agrees with an existing terminal marker.
func VerifyRejection(record Record, result api.CommitResult) error {
	if err := record.Validate(); err != nil {
		return err
	}
	if err := verifyRejection(record.Request.Key(), result); err != nil {
		return err
	}
	if record.Rejection != nil && *record.Rejection != result {
		return fmt.Errorf("%w: saved rejection differs", ErrConflict)
	}
	return nil
}

func verifyRejection(key api.RequestKey, result api.CommitResult) error {
	expected := api.CommitResult{Code: result.Code, RequestID: key.RequestID, Conflict: result.Conflict}
	switch result.Code {
	case api.CodeRejected:
		if result.Conflict != "" {
			return fmt.Errorf("%w: rejection contains a conflict reason", ErrConflict)
		}
	case api.CodeConflict:
		if result.Conflict != api.ConflictIdempotency && result.Conflict != api.ConflictPrecondition {
			return fmt.Errorf("%w: unsupported conflict reason", ErrConflict)
		}
	default:
		return fmt.Errorf("%w: response is not a conclusive rejection", ErrConflict)
	}
	// Rejections have no publication, transaction, roots or digests, even if a
	// supplied digest happens to match this command.
	if result != expected {
		return fmt.Errorf("%w: rejection identity or publication fields", ErrConflict)
	}
	return nil
}

func VerifyAcknowledgement(record Record, result api.CommitResult) error {
	if err := record.Validate(); err != nil {
		return err
	}
	if record.Rejection != nil || result.Code != api.CodeDurableAck ||
		result.RequestID != record.Request.RequestID || result.RequestDigest != record.Digest ||
		result.Conflict != "" || result.Fence.Store != record.StoreID ||
		result.Fence.LSN == 0 || result.Fence.Validate() != nil {
		return fmt.Errorf("%w: acknowledgement does not prove this command committed", ErrConflict)
	}
	for _, digest := range []model.ObjectID{result.ResultDigest, result.StateRoot, result.RootSet} {
		if digest != "" && !model.ValidDigest(string(digest)) {
			return fmt.Errorf("%w: invalid acknowledgement digest", ErrConflict)
		}
	}
	return nil
}

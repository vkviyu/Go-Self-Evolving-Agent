// Package aterequest preserves native ATE command identities and original bytes
// until their outcomes are known. Persistence implementations live in adapters.
package aterequest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/model"
)

const (
	MaxListRecords = 128
	MaxListBytes   = 32 << 20
	// MaxRecordBytes accommodates a maximum native body encoded as base64,
	// plus the bounded identities and expected fence.
	MaxRecordBytes = 12 << 20
)

var (
	ErrInvalid  = errors.New("invalid ATE pending request")
	ErrCorrupt  = errors.New("corrupt ATE pending request")
	ErrConflict = errors.New("ATE pending request identity conflict")
	ErrNotFound = errors.New("ATE pending request not found")
	ErrLimit    = errors.New("ATE pending request list limit")
	ErrRetired  = errors.New("ATE request retired without submission")
)

// Record owns its request bytes. Attempted and Rejection are durable lifecycle
// metadata supplied by the store, not part of the immutable command envelope.
type Record struct {
	StoreID   model.StoreID
	Request   api.CommitRequest
	Digest    model.ObjectID
	Attempted bool
	Rejection *api.CommitResult
}

// PendingStore must durably preserve exact envelopes, isolate returned memory,
// and serialize mutations of a request key across all callers.
type PendingStore interface {
	// Insert is idempotent only for an identical pending envelope. It must not
	// overwrite lifecycle metadata. Changed envelopes return ErrConflict;
	// rejected/retired keys remain reserved after payload deletion and return
	// ErrRejected/ErrRetired for identical envelopes, ErrConflict otherwise.
	Insert(context.Context, Record) error
	Get(context.Context, api.RequestKey) (Record, bool, error)
	// List orders keys lexicographically by (Space, Principal, RequestID).
	// After is exclusive. Both count and total EncodeRecord byte bounds apply.
	// If the first eligible record cannot fit, return ErrLimit, not an empty
	// page that would falsely imply recovery has finished.
	List(context.Context, ListOptions) ([]Record, error)
	// BeginAttempt atomically checks the exact saved envelope and durably marks
	// it attempted before returning true. Only one caller may acquire it.
	// A previous attempt returns false; terminal keys never acquire an attempt.
	BeginAttempt(context.Context, Record) (bool, error)
	// DeleteAcknowledged verifies the receipt and exact stored envelope before
	// deleting payload bytes. Retrying the same verified cleanup is idempotent.
	DeleteAcknowledged(context.Context, Record, api.CommitResult) error
	// DeleteRejected requires a persisted attempt and a verified response.
	// Persist a body-free rejection marker BEFORE deleting the payload. If
	// deletion fails, Get must attach that rejection for local-only cleanup.
	// The marker survives deletion and permanently prevents reuse of the key.
	DeleteRejected(context.Context, Record, api.CommitResult) error
}

// RetirementStore is optional; recovery uses it to abandon a dependent command
// only after proving its prerequisite was conclusively rejected.
type RetirementStore interface {
	GetTerminal(context.Context, api.RequestKey) (Rejection, bool, error)
	// RetireUnattempted atomically compares any saved envelope, proves no
	// attempt started, records retirement and deletes the payload. It must
	// serialize with Insert and BeginAttempt. A missing envelope may be
	// retired (recovery can fail between paired inserts); an attempted or
	// different envelope returns ErrConflict. Repeating the same retirement
	// succeeds. Deletion failure rolls back the entire retirement.
	RetireUnattempted(context.Context, Record) error
}

type ListOptions struct {
	After    *api.RequestKey
	Limit    int
	MaxBytes int
}

func (options ListOptions) Validate() error {
	if options.Limit <= 0 || options.Limit > MaxListRecords ||
		options.MaxBytes <= 0 || options.MaxBytes > MaxListBytes {
		return ErrLimit
	}
	if options.After != nil {
		if err := options.After.Validate(); err != nil {
			return fmt.Errorf("%w: list cursor: %w", ErrInvalid, err)
		}
	}
	return nil
}

// ListPending requires a complete bounded inventory. A short byte-limited page
// does not prove exhaustion, so check the exclusive cursor before admitting work.
func ListPending(ctx context.Context, pending PendingStore) ([]Record, error) {
	records, err := pending.List(ctx, ListOptions{Limit: MaxListRecords, MaxBytes: MaxListBytes})
	if err != nil {
		return nil, err
	}
	if len(records) > MaxListRecords {
		return nil, ErrLimit
	}
	total := 0
	var previous api.RequestKey
	for i, record := range records {
		raw, err := EncodeRecord(record)
		if err != nil {
			return nil, err
		}
		total += len(raw)
		if total > MaxListBytes {
			return nil, ErrLimit
		}
		key := record.Request.Key()
		if i > 0 && compareKeys(previous, key) >= 0 {
			return nil, ErrCorrupt
		}
		previous = key
	}
	if len(records) != 0 {
		next, err := pending.List(ctx, ListOptions{After: &previous, Limit: 1, MaxBytes: MaxListBytes})
		if err != nil {
			return nil, err
		}
		if len(next) != 0 {
			return nil, ErrLimit
		}
	}
	return records, nil
}

func compareKeys(a, b api.RequestKey) int {
	for _, pair := range [][2]string{{string(a.Space), string(b.Space)}, {string(a.Principal), string(b.Principal)}, {string(a.RequestID), string(b.RequestID)}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	return 0
}

// NewRecord computes ATE's canonical digest without interpreting or rewriting
// the opaque body. Neither Body nor ExpectedFence aliases caller-owned memory.
func NewRecord(storeID model.StoreID, request api.CommitRequest) (Record, error) {
	record := Record{StoreID: storeID, Request: request}
	digest, err := api.CommandDigest(request)
	if err != nil {
		return Record{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	record.Digest = digest
	if err := record.Validate(); err != nil {
		return Record{}, err
	}
	record.Request.Body = bytes.Clone(request.Body)
	if request.ExpectedFence != nil {
		fence := *request.ExpectedFence
		record.Request.ExpectedFence = &fence
	}
	return record, nil
}

func (record Record) Validate() error {
	if !validStoreID(record.StoreID) {
		return fmt.Errorf("%w: store identity", ErrInvalid)
	}
	digest, err := api.CommandDigest(record.Request)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if digest != record.Digest {
		return fmt.Errorf("%w: canonical command digest mismatch", ErrInvalid)
	}
	if fence := record.Request.ExpectedFence; fence != nil && fence.Store != record.StoreID {
		return fmt.Errorf("%w: expected fence belongs to another store", ErrInvalid)
	}
	if record.Rejection != nil {
		if !record.Attempted {
			return fmt.Errorf("%w: rejection without an attempt", ErrInvalid)
		}
		if err := verifyRejection(record.Request.Key(), *record.Rejection); err != nil {
			return err
		}
	}
	return nil
}

// Equal compares the exact immutable envelope, not its mutable attempt or
// rejection state. Comparing bytes as well as the digest prevents a corrupt
// digest field from disguising a changed payload.
func Equal(a, b Record) bool {
	if a.StoreID != b.StoreID || a.Digest != b.Digest ||
		a.Request.Key() != b.Request.Key() || a.Request.Method != b.Request.Method ||
		!bytes.Equal(a.Request.Body, b.Request.Body) {
		return false
	}
	af, bf := a.Request.ExpectedFence, b.Request.ExpectedFence
	return af == nil && bf == nil || af != nil && bf != nil && *af == *bf
}

type recordWire struct {
	Version int               `json:"version"`
	StoreID model.StoreID     `json:"store_id"`
	Request api.CommitRequest `json:"request"`
	Digest  model.ObjectID    `json:"digest"`
}

// EncodeRecord encodes only the immutable envelope. Adapters persist attempt
// state separately and attach any terminal rejection when loading a record.
func EncodeRecord(record Record) ([]byte, error) {
	if err := record.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(recordWire{Version: 1, StoreID: record.StoreID, Request: record.Request, Digest: record.Digest})
}

func DecodeRecord(raw []byte) (Record, error) {
	var wire recordWire
	if err := decodeCanonical(raw, MaxRecordBytes, &wire); err != nil {
		return Record{}, err
	}
	if wire.Version != 1 {
		return Record{}, fmt.Errorf("%w: unsupported record version", ErrCorrupt)
	}
	record := Record{StoreID: wire.StoreID, Request: wire.Request, Digest: wire.Digest}
	if err := record.Validate(); err != nil {
		return Record{}, fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
	return record, nil
}

// These are internal versioned encodings, not permissive transport JSON. Exact
// re-encoding rejects duplicate/unknown fields (including nested ones), field
// aliases, omitted fields, invalid UTF-8, trailing values and lossy decoding.
// Opaque Body bytes are base64 and never undergo JSON normalization.
func decodeCanonical(raw []byte, limit int, value any) error {
	if len(raw) == 0 || len(raw) > limit {
		return fmt.Errorf("%w: encoded size", ErrCorrupt)
	}
	if err := json.Unmarshal(raw, value); err != nil {
		return fmt.Errorf("%w: JSON: %v", ErrCorrupt, err)
	}
	encoded, err := json.Marshal(value)
	if err != nil || !bytes.Equal(raw, encoded) {
		return fmt.Errorf("%w: noncanonical encoding", ErrCorrupt)
	}
	return nil
}

func validStoreID(id model.StoreID) bool {
	return id != "" && len(id) <= api.MaxIdentityBytes && utf8.ValidString(string(id))
}

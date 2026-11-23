// Package atebridge records GSEA execution semantics in native ATE records.
package atebridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateports"
	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
)

// Process lends a client only for the callback's lifetime.
type Process interface {
	WithClient(context.Context, func(*client.Client) error) error
}

type Options struct {
	Process Process
	// Pending owns only unresolved commands. State must be backed by the same
	// ATE authority and pins prerequisites before a bridge lease is acquired.
	// Control metadata is read directly through the leased client.
	Pending aterequest.PendingStore
	State   ateports.StateStore
	// Namespace is a stable semantic host identity.
	Namespace        string
	OperationalSpace model.SpaceID
}

type Bridge struct {
	process          Process
	state            ateports.StateStore
	namespace        string
	gate             chan struct{}
	pending          aterequest.PendingStore
	operationalSpace model.SpaceID
}

var (
	ErrStoreChanged     = errors.New("atebridge: ATE store identity changed")
	ErrNamespaceChanged = errors.New("atebridge: execution namespace changed")
	ErrBacklog          = errors.New("atebridge: pending request scan exceeds 10000 records")
	ErrOutcomeUnknown   = errors.New("atebridge: original request outcome remains unknown")
)

// Serialize bridges sharing a namespace within this process. Cross-process
// ownership is supplied by Process; ATE remains the durable arbiter.
var gates sync.Map

func New(o Options) (*Bridge, error) {
	if nilInterface(o.Process) || strings.TrimSpace(o.Namespace) != o.Namespace || o.Namespace == "" || len(o.Namespace) > 256 || !utf8.ValidString(o.Namespace) {
		return nil, errors.New("atebridge: process and a bounded stable namespace are required")
	}
	if nilInterface(o.Pending) || nilInterface(o.State) {
		return nil, errors.New("atebridge: Pending and ATE-backed State required")
	}
	space := o.OperationalSpace
	if space == "" {
		space = model.SpaceID("gsea-execution-" + digest(o.Namespace))
	}
	if err := (api.RequestKey{Space: space, Principal: "gsea", RequestID: "validate"}).Validate(); err != nil {
		return nil, err
	}
	gateKey := "native/" + digest(o.Namespace, string(space))
	gate, _ := gates.LoadOrStore(gateKey, func() chan struct{} {
		ch := make(chan struct{}, 1)
		ch <- struct{}{}
		return ch
	}())
	return &Bridge{process: o.Process, state: o.State, namespace: o.Namespace, gate: gate.(chan struct{}),
		pending: o.Pending, operationalSpace: space}, nil
}

func nilInterface(v any) bool {
	if v == nil {
		return true
	}
	r := reflect.ValueOf(v)
	switch r.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return r.IsNil()
	}
	return false
}

// WithClient lends a bounded client after verifying its identity. Binding may
// recover metadata, so reads share the mutation gate. Callbacks must not retain
// the client, call State, or recursively call a bridge method.
func (b *Bridge) WithClient(ctx context.Context, fn func(*client.Client) error) error {
	return b.withMutation(ctx, fn)
}

// Execute serializes local mutations. A key permanently names its first exact
// request; build is called only when no request has been durably inserted.
// Unknown outcomes require calling Execute again with that SAME key.
func (b *Bridge) Execute(ctx context.Context, key string, build func(*client.Client) (api.CommitRequest, error)) (api.CommitResult, error) {
	return b.ExecuteInSpace(ctx, b.OperationalSpace(), key, build)
}

// ExecuteInSpace names the complete native request key before calling build.
// Native Execute defaults to OperationalSpace; cross-space callers must use
// this method so acknowledged commands never require body reconstruction.
func (b *Bridge) ExecuteInSpace(ctx context.Context, space model.SpaceID, key string, build func(*client.Client) (api.CommitRequest, error)) (api.CommitResult, error) {
	results := make(chan api.CommitResult, 1)
	err := b.withMutation(ctx, func(c *client.Client) error {
		var result api.CommitResult
		var err error
		result, err = b.executeNative(ctx, c, space, "caller/"+key, build)
		results <- result
		return err
	})
	select {
	case result := <-results:
		return result, err
	default:
		if err == nil {
			err = errors.New("atebridge: process returned without completing request")
		}
		return api.CommitResult{}, err
	}
}

// withMutation serializes a native mutation and lends the authenticated
// client only for the duration of the callback.
func (b *Bridge) withMutation(ctx context.Context, fn func(*client.Client) error) error {
	if ctx == nil || fn == nil {
		return errors.New("atebridge: context and mutation callback required")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.gate:
	}
	var entered, returned bool
	var mu sync.Mutex
	done := make(chan struct{})
	wrapped := func(c *client.Client) error {
		mu.Lock()
		if returned || entered {
			mu.Unlock()
			return errors.New("atebridge: client lease expired or callback already entered")
		}
		entered = true
		mu.Unlock()
		defer close(done)
		if err := b.bind(ctx, c); err != nil {
			return err
		}
		return fn(c)
	}
	// Track the whole lease, including namespace binding. A process timeout
	// during metadata recovery must not release the gate while it still runs.
	// Reject callbacks scheduled after the process returns, and release the
	// gate even when the process propagates a callback panic.
	defer func() {
		mu.Lock()
		returned = true
		hasEntered := entered
		mu.Unlock()
		if !hasEntered {
			b.gate <- struct{}{}
			return
		}
		select {
		case <-done:
			b.gate <- struct{}{}
		default:
			go func() {
				<-done
				b.gate <- struct{}{}
			}()
		}
	}()
	return b.process.WithClient(ctx, wrapped)
}

// OperationalSpace is execution history, not a semantic classification.
func (b *Bridge) OperationalSpace() model.SpaceID {
	return b.operationalSpace
}

func digest(parts ...string) string {
	raw, _ := json.Marshal(parts)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (b *Bridge) localKey(kind, key string) string {
	return "atebridge/v1/" + b.namespace + "/" + kind + "/" + digest(key)
}

func (b *Bridge) bind(ctx context.Context, c *client.Client) error {
	if c.Identity().Principal != "gsea" {
		return fmt.Errorf("atebridge: expected authenticated principal gsea")
	}
	return b.bindNative(ctx, c)
}

// bindNative publishes only namespace metadata, using the same deterministic
// pending/receipt recovery as every other bridge command. No State callback,
// random transaction identity, command mirror, or nested lease is involved.
func (b *Bridge) bindNative(ctx context.Context, c *client.Client) error {
	space := model.SpaceID("gsea-execution-" + digest(b.namespace))
	key := []byte("gsea/bridge-config/v1/" + digest(b.namespace))
	raw, _ := json.Marshal(b.OperationalSpace())
	value, err := c.GetState(ctx, api.ReadStateRequest{Space: space, Key: key})
	if err != nil {
		return err
	}
	if value.Found && (value.Value.Kind != api.ValueBytes || !bytes.Equal(value.Value.Data, raw)) {
		return errors.Join(ErrNamespaceChanged, ateports.ErrConflict)
	}
	_, err = b.executeNative(ctx, c, space, "namespace-binding", func(*client.Client) (api.CommitRequest, error) {
		body, err := api.EncodeStateBatch(api.StateBatch{Operations: []api.StateMutation{{
			Key: key, Kind: api.MutationPut, Value: api.Value{Kind: api.ValueBytes, Data: raw},
			Condition: api.ValueCondition{Kind: api.ConditionAbsent},
		}}})
		return api.CommitRequest{Space: space, Method: api.MethodMutateState, Body: body}, err
	})
	if err != nil {
		return err
	}
	value, err = c.GetState(ctx, api.ReadStateRequest{Space: space, Key: key})
	if err != nil {
		return err
	}
	if !value.Found || value.Value.Kind != api.ValueBytes || !bytes.Equal(value.Value.Data, raw) {
		return errors.Join(ErrNamespaceChanged, ateports.ErrConflict)
	}
	return nil
}

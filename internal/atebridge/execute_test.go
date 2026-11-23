package atebridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateports"
	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
)

type forbiddenBridgeState struct{}

func (forbiddenBridgeState) Get(context.Context, string) (json.RawMessage, bool, error) {
	return nil, false, errors.New("bridge must read native control metadata through its leased client")
}

func (forbiddenBridgeState) Put(context.Context, string, json.RawMessage) error {
	return errors.New("bridge must write native control metadata through its leased client")
}

func TestNativeMetadataRecoversWithoutRecoveryOrStateLease(t *testing.T) {
	f := newFixture(t)
	options := f.nativeOptions(t)
	options.State = forbiddenBridgeState{}
	var err error
	f.b, err = New(options)
	if err != nil {
		t.Fatal(err)
	}
	f.p.drop = "after"
	calls := 0
	callback := func(*client.Client) error { calls++; return nil }
	if err := f.b.WithClient(t.Context(), callback); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("metadata commit did not retain unknown outcome: %v", err)
	}
	pending := f.pendingRequests(t)
	if calls != 0 || len(pending) != 1 {
		t.Fatal("unbound callback ran or metadata recovery was lost")
	}
	f.restart(t)
	options = f.nativeOptions(t)
	options.State = forbiddenBridgeState{}
	f.b, err = New(options)
	if err != nil {
		t.Fatal(err)
	}
	commits := len(f.p.commits)
	if err := f.b.WithClient(t.Context(), callback); err != nil || calls != 1 ||
		len(f.pendingRequests(t)) != 0 || len(f.p.commits) != commits {
		t.Fatalf("native binding failed to recover without Sync: %v", err)
	}
	options.OperationalSpace = "different-execution-partition"
	changed, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := changed.WithClient(t.Context(), callback); !errors.Is(err, ateports.ErrConflict) || calls != 1 {
		t.Fatalf("semantic namespace silently rebound: %v", err)
	}
}

func TestNativeUnknownRecoveryAndAcknowledgedLookupNeverRebuild(t *testing.T) {
	for _, drop := range []string{"before", "after"} {
		t.Run(drop, func(t *testing.T) {
			f := newFixture(t)
			f.useNative(t)
			if err := f.b.RecoverPending(t.Context()); err != nil {
				t.Fatal(err)
			}
			f.p.drop = drop
			builds := 0
			_, err := f.b.Execute(t.Context(), "original", func(*client.Client) (api.CommitRequest, error) {
				builds++
				return stateRequest(t, f.b, "original bytes"), nil
			})
			if !errors.Is(err, ErrOutcomeUnknown) {
				t.Fatalf("lost response classification: %v", err)
			}
			pending := f.pendingRequests(t)
			if len(pending) != 1 || !pending[0].Attempted {
				t.Fatal("unknown request lost its exact attempted envelope")
			}
			original := pending[0]
			f.restart(t)
			f.useNative(t)
			count := len(f.p.commits)
			result, err := f.b.Execute(t.Context(), "original", func(*client.Client) (api.CommitRequest, error) {
				builds++
				t.Error("rebuilt original command")
				return stateRequest(t, f.b, "replacement"), nil
			})
			if builds != 1 || len(f.p.commits) != count {
				t.Fatal("recovery rebuilt or retransmitted an attempted command")
			}
			if drop == "before" {
				remaining := f.pendingRequests(t)
				if !errors.Is(err, ErrOutcomeUnknown) || len(remaining) != 1 || !aterequest.Equal(original, remaining[0]) {
					t.Fatalf("ambiguous absence lost recovery bytes: %v", err)
				}
				return
			}
			if err != nil || result.Code != api.CodeDurableAck || len(f.pendingRequests(t)) != 0 {
				t.Fatalf("durable recovery failed to delete bytes: %+v %v", result, err)
			}
			f.restart(t)
			f.useNative(t)
			again, err := f.b.Execute(t.Context(), "original", func(*client.Client) (api.CommitRequest, error) {
				t.Error("acknowledged command rebuilt after pending cleanup")
				return api.CommitRequest{}, errors.New("unexpected build")
			})
			if err != nil || result != again {
				t.Fatalf("native receipt replay: %+v %v", again, err)
			}
		})
	}
}

type failNativeCleanup struct {
	aterequest.PendingStore
	fail bool
}

type failNativeAttempt struct{ aterequest.PendingStore }

func (s failNativeAttempt) BeginAttempt(context.Context, aterequest.Record) (bool, error) {
	return false, errors.New("injected failure before attempt admission")
}

func TestNativeRecoveryResumesUnattemptedBytesAndLeavesOtherOwners(t *testing.T) {
	f := newFixture(t)
	f.useNative(t)
	if err := f.b.RecoverPending(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.b.pending = failNativeAttempt{f.db.ATERequests()}
	start := len(f.p.commits)
	_, err := f.b.Execute(t.Context(), "unattempted", func(*client.Client) (api.CommitRequest, error) {
		return stateRequest(t, f.b, "saved before transmission"), nil
	})
	if !errors.Is(err, ErrOutcomeUnknown) || len(f.p.commits) != start {
		t.Fatalf("failed admission transmitted a command: %v", err)
	}
	pending := f.pendingRequests(t)
	if len(pending) != 1 || pending[0].Attempted {
		t.Fatal("unattempted recovery bytes missing")
	}
	foreignRequest := pending[0].Request
	foreignRequest.RequestID = "another-owner"
	foreign, err := aterequest.NewRecord(pending[0].StoreID, foreignRequest)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.ATERequests().Insert(t.Context(), foreign); err != nil {
		t.Fatal(err)
	}
	f.restart(t)
	f.useNative(t)
	if err := f.b.RecoverPending(t.Context()); err != nil {
		t.Fatal(err)
	}
	remaining := f.pendingRequests(t)
	if len(remaining) != 1 || !aterequest.Equal(remaining[0], foreign) ||
		len(f.p.commits) != start+1 || !reflect.DeepEqual(f.p.commits[start], pending[0].Request) {
		t.Fatal("recovery changed original bytes or touched another owner")
	}
}

func (s *failNativeCleanup) DeleteAcknowledged(ctx context.Context, record aterequest.Record, result api.CommitResult) error {
	if s.fail {
		s.fail = false
		return errors.New("injected acknowledgement cleanup failure")
	}
	return s.PendingStore.DeleteAcknowledged(ctx, record, result)
}

func TestNativeCleanupRecoveryDoesNotRepeatAcknowledgedCommand(t *testing.T) {
	f := newFixture(t)
	f.useNative(t)
	if err := f.b.RecoverPending(t.Context()); err != nil {
		t.Fatal(err)
	}
	// The StateStore uses the same process and aterequest but its own owner.
	// Fail only cleanup of this bridge's request.
	f.b.pending = &failNativeCleanup{PendingStore: f.db.ATERequests(), fail: true}
	result, err := f.b.Execute(t.Context(), "cleanup", func(*client.Client) (api.CommitRequest, error) {
		return stateRequest(t, f.b, "native"), nil
	})
	if !errors.Is(err, aterequest.ErrCleanup) || errors.Is(err, ErrOutcomeUnknown) || result.Code != api.CodeDurableAck {
		t.Fatalf("cleanup confused with unknown outcome: %+v %v", result, err)
	}
	if len(f.pendingRequests(t)) != 1 {
		t.Fatal("cleanup failure did not retain exact pending bytes")
	}
	count := len(f.p.commits)
	if err := f.b.RecoverPending(t.Context()); err != nil || len(f.pendingRequests(t)) != 0 || len(f.p.commits) != count {
		t.Fatalf("Sync failed to reconcile without reexecuting: %v", err)
	}

}

func TestNativeExplicitSpaceAndModeValidation(t *testing.T) {
	f := newFixture(t)
	options := f.nativeOptions(t)
	for _, mutate := range []func(*Options){
		func(o *Options) { o.Pending = nil },
		func(o *Options) { o.State = nil },
		func(o *Options) { o.Namespace = " " },
	} {
		bad := options
		mutate(&bad)
		if _, err := New(bad); err == nil {
			t.Fatal("accepted ambiguous native configuration")
		}
	}
	f.useNative(t)
	space := model.SpaceID("native-knowledge-target")
	first, err := f.b.ExecuteInSpace(t.Context(), space, "cross-space", func(*client.Client) (api.CommitRequest, error) {
		request := stateRequest(t, f.b, "knowledge")
		request.Space = space
		return request, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	f.restart(t)
	f.useNative(t)
	again, err := f.b.ExecuteInSpace(t.Context(), space, "cross-space", func(*client.Client) (api.CommitRequest, error) {
		t.Error("cross-space receipt required rebuilding")
		return api.CommitRequest{}, errors.New("unexpected build")
	})
	if err != nil || first != again || len(f.pendingRequests(t)) != 0 {
		t.Fatalf("cross-space recovery: %+v %v", again, err)
	}
}

func stateRequest(t *testing.T, b *Bridge, value string) api.CommitRequest {
	t.Helper()
	body, err := api.EncodeStateBatch(api.StateBatch{Operations: []api.StateMutation{{
		Key: []byte("exact"), Kind: api.MutationPut, Value: api.Value{Kind: api.ValueBytes, Data: []byte(value)},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return api.CommitRequest{Space: b.OperationalSpace(), Method: api.MethodMutateState, Body: body}
}

func TestExecuteSerializesAcrossBridgeInstances(t *testing.T) {
	f := newFixture(t)
	other, err := New(f.nativeOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	builds := 0
	errs := make(chan error, 12)
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b := f.b
			if i%2 == 1 {
				b = other
			}
			_, err := b.Execute(context.Background(), "concurrent", func(*client.Client) (api.CommitRequest, error) {
				mu.Lock()
				builds++
				mu.Unlock()
				return stateRequest(t, b, "once"), nil
			})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if builds != 1 || len(f.p.commits) != 2 {
		t.Fatalf("builders=%d commits=%d", builds, len(f.p.commits))
	}
	if !bytes.Equal(f.p.commits[1].Body, stateRequest(t, f.b, "once").Body) {
		t.Fatal("body changed")
	}
}

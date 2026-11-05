package tool

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

type snapshotMemoryStore struct {
	mu         sync.Mutex
	manifests  map[string][]byte
	references map[SnapshotReference]string
	failure    error
}

func newSnapshotMemoryStore() *snapshotMemoryStore {
	return &snapshotMemoryStore{manifests: make(map[string][]byte), references: make(map[SnapshotReference]string)}
}

func (store *snapshotMemoryStore) Save(_ context.Context, manifest SnapshotManifest, reference SnapshotReference) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.failure != nil {
		return store.failure
	}
	id, err := manifest.ID()
	if err != nil {
		return err
	}
	encoded, err := manifest.Encode()
	if err != nil {
		return err
	}
	if previous := store.references[reference]; previous != "" && previous != id {
		return ErrSnapshotReferenceConflict
	}
	store.manifests[id], store.references[reference] = encoded, id
	return nil
}

func (store *snapshotMemoryStore) Load(_ context.Context, identity string) (SnapshotManifest, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.failure != nil {
		return SnapshotManifest{}, store.failure
	}
	encoded, ok := store.manifests[identity]
	if !ok {
		return SnapshotManifest{}, ErrSnapshotUnavailable
	}
	return DecodeSnapshotManifest(encoded)
}

func TestSnapshotRestoreIsOwnedImmutableAndDoesNotActivate(t *testing.T) {
	ctx := context.Background()
	store := newSnapshotMemoryStore()
	oldBinding := testBinding("v1", "old")
	oldBinding.ImplementationID = "artifact-v1"
	old := identityRegistry(t, oldBinding)
	if err := old.ConfigurePersistence(store, nil); err != nil {
		t.Fatal(err)
	}
	if err := old.RetainSnapshot(ctx, old.Snapshot(), SnapshotReference{"turn", "session", "turn"}); err != nil {
		t.Fatal(err)
	}
	identity := requireSnapshotID(t, old)
	newBinding := testBinding("v2", "new")
	newBinding.ImplementationID = "artifact-v2"
	current := identityRegistry(t, newBinding)
	active := current.Snapshot()
	if err := current.ConfigurePersistence(store, func(_ context.Context, entry SnapshotEntry) (Binding, error) { return oldBinding, nil }); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	results := make(chan *Snapshot, 8)
	errorsSeen := make(chan error, 8)
	for range 8 {
		group.Go(func() {
			snapshot, err := current.ResolveSnapshot(ctx, identity)
			results <- snapshot
			errorsSeen <- err
		})
	}
	group.Wait()
	close(results)
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	var restored *Snapshot
	for snapshot := range results {
		if restored != nil && restored != snapshot {
			t.Fatal("concurrent restore retained duplicate snapshots")
		}
		restored = snapshot
	}
	if !current.OwnsSnapshot(restored) || current.Snapshot() != active {
		t.Fatal("restore replaced active snapshot or foreign ownership")
	}
	if local, ok := current.SnapshotAt(restored.Generation()); !ok || local != restored {
		t.Fatal("restored generation not owned")
	}
	if _, err := current.Activate(testBinding("v3", "third")); err != nil {
		t.Fatal(err)
	}
	if current.Snapshot().Generation() <= restored.Generation() {
		t.Fatal("new publication reused imported generation")
	}
	if binding, ok := restored.Get("echo"); !ok || binding.Descriptor.Revision != "v1" {
		t.Fatal("restored snapshot mutated")
	}
	if err := current.RetainSnapshot(ctx, old.Snapshot(), SnapshotReference{"turn", "session", "foreign"}); !errors.Is(err, ErrForeignSnapshot) {
		t.Fatalf("foreign snapshot retained: %v", err)
	}
}

func TestSnapshotRestoreRejectsDriftWithoutFallback(t *testing.T) {
	ctx := context.Background()
	binding := testBinding("v1", "one")
	original := identityRegistry(t, binding)
	manifest, err := original.Snapshot().Manifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"missing", "host", "native", "resolver", "metadata", "storage", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			store := newSnapshotMemoryStore()
			candidate, err := original.Snapshot().Manifest()
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "host" {
				candidate.Host = strings.Repeat("0", 64)
			}
			id, err := candidate.ID()
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "missing" {
				if err := store.Save(ctx, candidate, SnapshotReference{"turn", "session", "turn"}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "storage" {
				store.failure = errors.New("read failure")
			}
			registry := identityRegistry(t, testBinding("v2", "two"))
			var resolver BindingResolver
			if scenario == "resolver" || scenario == "metadata" {
				wrong := binding
				if scenario == "resolver" {
					wrong.ImplementationID = "different code"
				} else {
					wrong.Descriptor.RiskClass = RiskWrite
				}
				resolver = func(context.Context, SnapshotEntry) (Binding, error) { return wrong, nil }
			}
			if err := registry.ConfigurePersistence(store, resolver); err != nil {
				t.Fatal(err)
			}
			active := registry.Snapshot()
			callCtx := ctx
			if scenario == "canceled" {
				var cancel context.CancelFunc
				callCtx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if _, err := registry.ResolveSnapshot(callCtx, id); err == nil {
				t.Fatal("resolved unavailable/drifting snapshot")
			}
			if registry.Snapshot() != active || len(registry.snapshots) != 2 {
				t.Fatal("failure mutated registry")
			}
		})
	}
	encoded, err := manifest.Encode()
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range [][]byte{append(append([]byte(nil), encoded...), []byte(` {}`)...), []byte(strings.Replace(string(encoded), `"Version":1`, `"Version":2`, 1)), []byte(strings.Replace(string(encoded), `"Host":`, `"Unknown":`, 1))} {
		if _, err := DecodeSnapshotManifest(invalid); err == nil {
			t.Fatal("decoded invalid manifest")
		}
	}
	manifest.Tools = append(manifest.Tools, manifest.Tools[0])
	if _, err := manifest.Encode(); err == nil {
		t.Fatal("accepted duplicate tool name")
	}
}

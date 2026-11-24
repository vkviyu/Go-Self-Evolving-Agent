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
	oldBinding.ImplementationID = "native-fixture"
	old := identityRegistry(t, oldBinding)
	if err := old.ConfigurePersistence(store); err != nil {
		t.Fatal(err)
	}
	if err := old.RetainSnapshot(ctx, old.Snapshot(), SnapshotReference{"turn", "session", "turn"}); err != nil {
		t.Fatal(err)
	}
	identity := requireSnapshotID(t, old)
	extra := testBinding("v1", "extra")
	extra.Descriptor.Name = "extra"
	current := NewRegistry()
	if _, err := current.ActivateMany(oldBinding, extra); err != nil {
		t.Fatal(err)
	}
	active := current.Snapshot()
	if err := current.ConfigurePersistence(store); err != nil {
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
	if _, ok := restored.Get("extra"); ok {
		t.Fatal("restored view exposed a tool absent from the original turn")
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
	for _, scenario := range []string{"missing", "host", "revision", "removed", "implementation", "metadata", "storage", "canceled"} {
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
			registered := binding
			switch scenario {
			case "removed":
				registered.Descriptor.Name = "other"
			case "implementation":
				registered.ImplementationID = "different code"
			case "metadata":
				registered.Descriptor.RiskClass = RiskWrite
			default:
				registered.Descriptor.Revision = "v2"
			}
			registry := identityRegistry(t, registered)
			if err := registry.ConfigurePersistence(store); err != nil {
				t.Fatal(err)
			}
			active := registry.Snapshot()
			callCtx := ctx
			if scenario == "canceled" {
				var cancel context.CancelFunc
				callCtx, cancel = context.WithCancel(ctx)
				cancel()
			}
			want := ErrSnapshotUnavailable
			switch scenario {
			case "host":
				want = ErrSnapshotHostChanged
			case "storage":
				want = store.failure
			case "canceled":
				want = context.Canceled
			}
			if _, err := registry.ResolveSnapshot(callCtx, id); !errors.Is(err, want) {
				t.Fatalf("restore error = %v, want %v", err, want)
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

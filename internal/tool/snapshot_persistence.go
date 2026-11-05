package tool

import (
	"context"
	"errors"
	"fmt"
)

// ConfigurePersistence is a one-time composition-root operation, before any
// turns are admitted. Persistence/resolution never runs under the registry lock.
func (registry *Registry) ConfigurePersistence(store SnapshotStore, resolver BindingResolver) error {
	if store == nil {
		return errors.New("snapshot persistence requires a store")
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.snapshotStore != nil {
		return errors.New("snapshot persistence already configured")
	}
	registry.snapshotStore, registry.bindingResolver = store, resolver
	return nil
}

// RetainSnapshot must succeed before recording/dispatching work that depends
// on the snapshot. A crash afterward can leave an extra reference, never an
// executed operation whose prerequisite manifest was not committed.
func (registry *Registry) RetainSnapshot(ctx context.Context, snapshot *Snapshot, reference SnapshotReference) error {
	if err := reference.Validate(); err != nil {
		return err
	}
	if !registry.OwnsSnapshot(snapshot) {
		return ErrForeignSnapshot
	}
	registry.mu.Lock()
	store := registry.snapshotStore
	registry.mu.Unlock()
	if store == nil {
		return nil
	} // Explicitly ephemeral registries remain supported.
	manifest, err := snapshot.Manifest()
	if err != nil {
		return err
	}
	return store.Save(ctx, manifest, reference)
}

// ResolveSnapshot reconstructs only the requested content ID. Imported views
// become owned by this registry but never replace its current active snapshot.
func (registry *Registry) ResolveSnapshot(ctx context.Context, identity string) (*Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if snapshot, err := registry.SnapshotByID(identity); err == nil {
		return snapshot, nil
	}
	registry.mu.Lock()
	store, resolver := registry.snapshotStore, registry.bindingResolver
	registry.mu.Unlock()
	if store == nil {
		return nil, fmt.Errorf("%w: %s", ErrSnapshotUnavailable, identity)
	}
	manifest, err := store.Load(ctx, identity)
	if err != nil {
		return nil, fmt.Errorf("load runtime snapshot: %w", err)
	}
	id, err := manifest.ID()
	if err != nil || id != identity {
		return nil, fmt.Errorf("%w: manifest identity mismatch", ErrSnapshotUnavailable)
	}
	host, err := hostIdentity()
	if err != nil {
		return nil, err
	}
	if manifest.Host != host {
		return nil, fmt.Errorf("%w: host executable changed", ErrSnapshotUnavailable)
	}
	current := registry.Snapshot()
	bindings := make(map[string]Binding, len(manifest.Tools))
	for _, entry := range manifest.Tools {
		binding, ok := current.Get(entry.Descriptor.Name)
		if !ok || !entry.Matches(binding) {
			if resolver == nil {
				return nil, fmt.Errorf("%w: %s@%s", ErrSnapshotUnavailable, entry.Descriptor.Name, entry.Descriptor.Revision)
			}
			binding, err = resolver(ctx, entry)
			if err != nil {
				return nil, fmt.Errorf("restore %s@%s: %w", entry.Descriptor.Name, entry.Descriptor.Revision, err)
			}
		}
		if err := binding.validate(); err != nil {
			return nil, err
		}
		if !entry.Matches(binding) {
			return nil, fmt.Errorf("%w: reconstructed binding mismatch", ErrSnapshotUnavailable)
		}
		binding.Descriptor = cloneDescriptor(binding.Descriptor)
		bindings[entry.Descriptor.Name] = binding
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	candidate := &Snapshot{bindings: bindings}
	if id, err := candidate.ID(); err != nil || id != identity {
		return nil, ErrSnapshotUnavailable
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	for _, snapshot := range registry.snapshots {
		if id, err := snapshot.ID(); err == nil && id == identity {
			return snapshot, nil
		}
	}
	registry.generation++
	candidate.generation = registry.generation
	registry.snapshots[candidate.generation] = candidate
	return candidate, nil
}

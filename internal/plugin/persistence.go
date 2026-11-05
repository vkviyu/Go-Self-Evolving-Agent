package plugin

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	pluginwasm "github.com/vkviyu/go-self-evolving-agent/internal/plugin/wasm"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

// OpenManager restores a catalog while the caller holds exclusive runtime
// ownership. All blobs/limits are checked and modules compiled before any
// active plugin is published. Startup failure must abort runtime construction.
func OpenManager(ctx context.Context, runtime *pluginwasm.Runtime, registry *tool.Registry, catalog Catalog, reservedPrefixes ...string) (*Manager, error) {
	if catalog == nil {
		return nil, errors.New("persistent plugin manager requires a catalog")
	}
	manager, err := NewManager(runtime, registry)
	if err != nil {
		return nil, err
	}
	manager.catalog, manager.reservedPrefixes = catalog, append([]string(nil), reservedPrefixes...)
	stored, err := catalog.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load plugin catalog: %w", err)
	}
	succeeded := false
	defer func() {
		if !succeeded {
			_ = manager.Close(context.Background())
		}
	}()
	var active []tool.Binding
	var total int64
	for _, item := range stored {
		record := item.Record
		record.Manifest.Tool = cloneDescriptor(record.Manifest.Tool)
		if err := record.ValidateSource(item.Source); err != nil {
			return nil, err
		}
		total += int64(len(item.Source))
		if total > MaxCatalogBytes {
			return nil, errors.New("plugin catalog retained artifact limit exceeded")
		}
		if record.RuntimeIdentity != runtime.ResourceIdentity() {
			return nil, errors.New("plugin sandbox limits changed; refusing implicit revision migration")
		}
		if item.State != StateActive && item.State != StateStaged && item.State != StateRetired {
			return nil, ErrCatalogInvalid
		}
		if err := manager.checkName(record.Manifest, registry.Snapshot()); err != nil {
			return nil, err
		}
		key := artifactKey(record.Manifest.Tool.Name, record.Manifest.Tool.Revision)
		if manager.artifacts[key] != nil {
			return nil, ErrArtifactExists
		}
		module, err := runtime.Compile(ctx, pluginwasm.Revision{PluginID: record.Manifest.PluginID, Revision: record.Manifest.Tool.Revision, Timeout: record.Manifest.Timeout}, item.Source)
		if err != nil {
			return nil, err
		}
		artifact := &Artifact{owner: manager, manifest: record.Manifest, module: module, contentSHA: record.ContentSHA256, state: item.State, runtimeIdentity: record.RuntimeIdentity, sizeBytes: len(item.Source)}
		manager.artifacts[key] = artifact
		if item.State == StateActive {
			if manager.activeByTool[record.Manifest.Tool.Name] != nil {
				return nil, ErrCatalogInvalid
			}
			manager.activeByTool[record.Manifest.Tool.Name] = artifact
			active = append(active, artifact.binding())
		}
	}
	if len(active) > 0 {
		_, err := registry.ActivateManyWithCommit(func(previous, _ *tool.Snapshot) error {
			for _, binding := range active {
				if _, exists := previous.Get(binding.Descriptor.Name); exists {
					return ErrToolOwned
				}
			}
			return nil
		}, active...)
		if err != nil {
			return nil, err
		}
	}
	succeeded = true
	return manager, nil
}

func (artifact *Artifact) record() RevisionRecord {
	return RevisionRecord{Version: CatalogVersion, Manifest: artifact.Manifest(), ContentSHA256: artifact.contentSHA, RuntimeIdentity: artifact.runtimeIdentity}
}

func (artifact *Artifact) binding() tool.Binding {
	return tool.Binding{Descriptor: cloneDescriptor(artifact.manifest.Tool), Executor: artifact.module, ImplementationID: fmt.Sprintf("wasm:%s:timeout:%d:%s", artifact.contentSHA, artifact.manifest.Timeout, artifact.runtimeIdentity)}
}

// ResolveBinding exposes only an exact retained module to snapshot recovery.
// It does not promote the revision or grant permission to invoke it.
func (m *Manager) ResolveBinding(ctx context.Context, entry tool.SnapshotEntry) (tool.Binding, error) {
	if err := ctx.Err(); err != nil {
		return tool.Binding{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.mutationError(); err != nil {
		return tool.Binding{}, err
	}
	artifact := m.artifacts[artifactKey(entry.Descriptor.Name, entry.Descriptor.Revision)]
	if artifact == nil {
		return tool.Binding{}, tool.ErrSnapshotUnavailable
	}
	if err := m.checkName(artifact.manifest, m.registry.Snapshot()); err != nil {
		return tool.Binding{}, err
	}
	binding := artifact.binding()
	if !entry.Matches(binding) {
		return tool.Binding{}, tool.ErrSnapshotUnavailable
	}
	return binding, nil
}

// Caller holds Manager.mu. Publication hooks also hold Registry.mu and must
// use the supplied immutable snapshot rather than calling registry methods.
func (m *Manager) checkName(manifest Manifest, snapshot *tool.Snapshot) error {
	for _, prefix := range m.reservedPrefixes {
		if strings.HasPrefix(manifest.Tool.Name, prefix) {
			return fmt.Errorf("%w: reserved namespace %s", ErrToolOwned, prefix)
		}
	}
	for _, artifact := range m.artifacts {
		if artifact.manifest.Tool.Name == manifest.Tool.Name && artifact.manifest.PluginID != manifest.PluginID {
			return ErrToolOwned
		}
	}
	if current, exists := snapshot.Get(manifest.Tool.Name); exists {
		owned := m.activeByTool[manifest.Tool.Name]
		module, ok := current.Executor.(*pluginwasm.Module)
		if !ok || owned == nil || owned.module != module || owned.manifest.Tool.Revision != current.Descriptor.Revision {
			return ErrToolOwned
		}
	}
	return nil
}

func (m *Manager) mutationError() error {
	if m.closed {
		return ErrManagerClosed
	}
	return m.persistenceErr
}

func (m *Manager) persistenceFailure(cause error) error {
	// SQL commit errors may be ambiguous. Do not compensate or retry writes
	// against an in-memory state that might disagree with the durable catalog.
	m.persistenceErr = errors.Join(ErrPersistenceUncertain, cause)
	return m.persistenceErr
}

func (m *Manager) commitActive(name, next string) error {
	if m.catalog == nil {
		return nil
	}
	expected := ""
	if previous := m.activeByTool[name]; previous != nil {
		expected = previous.manifest.Tool.Revision
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.catalog.SetActive(ctx, name, expected, next); err != nil {
		return m.persistenceFailure(err)
	}
	return nil
}

// Retire removes only this manager's exact active revision from future turns.
// Code and metadata remain retained for pinned snapshots and explicit rollback.
func (m *Manager) Retire(name, revision string) (*tool.Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.mutationError(); err != nil {
		return nil, err
	}
	artifact := m.activeByTool[name]
	if artifact == nil || artifact.manifest.Tool.Revision != revision {
		return nil, tool.ErrToolNotFound
	}
	snapshot, err := m.registry.RetireManyWithCommit(func(previous, _ *tool.Snapshot) error {
		if err := m.checkName(artifact.manifest, previous); err != nil {
			return err
		}
		return m.commitActive(name, "")
	}, artifact.binding())
	if err != nil {
		return nil, err
	}
	artifact.state = StateRetired
	delete(m.activeByTool, name)
	return snapshot, nil
}

func (m *Manager) Artifact(name, revision string) (*Artifact, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	artifact, ok := m.artifacts[artifactKey(name, revision)]
	return artifact, ok
}

type InventoryEntry struct {
	Manifest        Manifest
	ContentSHA256   string
	State           State
	SizeBytes       int
	RetentionReason string
}

// Inventory returns immutable lifecycle information, not a deletion plan.
// Without durable snapshot reference tracking, even retired artifacts are
// retained. No automatic disk GC or in-process module unloading is performed.
func (m *Manager) Inventory() []InventoryEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	entries := make([]InventoryEntry, 0, len(m.artifacts))
	for _, artifact := range m.artifacts {
		reason := "retained for snapshots and explicit rollback; safe deletion is not established"
		if artifact.state == StateStaged {
			reason = "staged, awaiting explicit activation or discard policy"
		}
		if artifact.state == StateActive {
			reason = "active revision"
		}
		entries = append(entries, InventoryEntry{Manifest: artifact.Manifest(), ContentSHA256: artifact.contentSHA, State: artifact.state, SizeBytes: artifact.sizeBytes, RetentionReason: reason})
	}
	sort.Slice(entries, func(i, j int) bool {
		return artifactKey(entries[i].Manifest.Tool.Name, entries[i].Manifest.Tool.Revision) < artifactKey(entries[j].Manifest.Tool.Name, entries[j].Manifest.Tool.Revision)
	})
	return entries
}

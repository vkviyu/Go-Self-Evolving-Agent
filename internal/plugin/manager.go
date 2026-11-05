// Package plugin manages immutable WASM revisions above the wazero runtime and
// publishes them through the shared tool registry.
package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	pluginwasm "github.com/vkviyu/go-self-evolving-agent/internal/plugin/wasm"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

// State is an artifact's control-plane lifecycle state.
type State string

const (
	StateStaged  State = "staged"
	StateActive  State = "active"
	StateRetired State = "retired"
)

var (
	ErrArtifactExists  = errors.New("plugin artifact already exists")
	ErrArtifactForeign = errors.New("plugin artifact belongs to a different manager")
	ErrManagerClosed   = errors.New("plugin manager is closed")
)

// Manifest is the trusted metadata paired with one immutable WASM artifact.
// Tool.Revision is the registry revision and must never be reused for different
// bytes.
type Manifest struct {
	PluginID string
	Tool     tool.Descriptor
	Timeout  time.Duration
}

// Artifact is a compiled, validated plugin revision. Its executable and
// manifest are immutable after staging.
type Artifact struct {
	owner           *Manager
	manifest        Manifest
	module          *pluginwasm.Module
	contentSHA      string
	state           State
	runtimeIdentity string
	sizeBytes       int
}

// Manifest returns a defensive metadata copy.
func (a *Artifact) Manifest() Manifest {
	manifest := a.manifest
	manifest.Tool = cloneDescriptor(manifest.Tool)
	return manifest
}

// ContentSHA256 is the immutable source artifact identity.
func (a *Artifact) ContentSHA256() string { return a.contentSHA }

// State returns the current lifecycle state.
func (a *Artifact) State() State {
	if a == nil || a.owner == nil {
		return ""
	}
	a.owner.mu.Lock()
	defer a.owner.mu.Unlock()
	return a.state
}

// Manager stages immutable modules and atomically publishes registry revisions.
// Retired artifacts remain compiled so in-flight snapshots and explicit
// rollback stay valid. A future lease tracker can drain them before Close.
type Manager struct {
	mu               sync.Mutex
	runtime          *pluginwasm.Runtime
	registry         *tool.Registry
	artifacts        map[string]*Artifact
	activeByTool     map[string]*Artifact
	closed           bool
	catalog          Catalog
	reservedPrefixes []string
	persistenceErr   error
}

// NewManager creates an empty plugin lifecycle manager.
func NewManager(runtime *pluginwasm.Runtime, registry *tool.Registry) (*Manager, error) {
	if runtime == nil || registry == nil {
		return nil, errors.New("plugin manager requires WASM runtime and tool registry")
	}
	return &Manager{
		runtime:      runtime,
		registry:     registry,
		artifacts:    make(map[string]*Artifact),
		activeByTool: make(map[string]*Artifact),
	}, nil
}

// Stage validates and compiles a candidate without changing the active tool
// snapshot. A revision identity cannot be reused, even for identical bytes.
func (m *Manager) Stage(ctx context.Context, manifest Manifest, source []byte) (*Artifact, error) {
	if manifest.PluginID == "" {
		return nil, errors.New("plugin ID is required")
	}
	if err := manifest.Tool.Validate(); err != nil {
		return nil, err
	}
	if len(source) == 0 || len(source) > MaxArtifactBytes {
		return nil, errors.New("plugin WASM artifact must be between 1 byte and 32 MiB")
	}
	if manifest.Timeout < 0 {
		return nil, errors.New("plugin timeout cannot be negative")
	}
	if manifest.Timeout == 0 {
		manifest.Timeout = m.runtime.DefaultTimeout()
	}
	manifest.Tool = cloneDescriptor(manifest.Tool)
	source = append([]byte(nil), source...)
	sum := sha256.Sum256(source)
	record := RevisionRecord{
		Version: CatalogVersion, Manifest: manifest,
		ContentSHA256: hex.EncodeToString(sum[:]), RuntimeIdentity: m.runtime.ResourceIdentity(),
	}
	if _, err := record.Encode(); err != nil {
		return nil, err
	}
	key := artifactKey(manifest.Tool.Name, manifest.Tool.Revision)
	m.mu.Lock()
	if err := m.mutationError(); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if err := m.checkName(manifest, m.registry.Snapshot()); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if _, exists := m.artifacts[key]; exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrArtifactExists, key)
	}
	m.mu.Unlock()

	module, err := m.runtime.Compile(ctx, pluginwasm.Revision{
		PluginID: manifest.PluginID,
		Revision: manifest.Tool.Revision,
		Timeout:  manifest.Timeout,
	}, source)
	if err != nil {
		return nil, err
	}
	artifact := &Artifact{
		owner:           m,
		manifest:        Manifest{PluginID: manifest.PluginID, Tool: cloneDescriptor(manifest.Tool), Timeout: manifest.Timeout},
		module:          module,
		contentSHA:      record.ContentSHA256,
		state:           StateStaged,
		runtimeIdentity: m.runtime.ResourceIdentity(),
		sizeBytes:       len(source),
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.mutationError(); err != nil {
		_ = module.Close(context.Background())
		return nil, err
	}
	if _, exists := m.artifacts[key]; exists {
		_ = module.Close(context.Background())
		return nil, fmt.Errorf("%w: %s", ErrArtifactExists, key)
	}
	if err := m.checkName(manifest, m.registry.Snapshot()); err != nil {
		_ = module.Close(context.Background())
		return nil, err
	}
	if m.catalog != nil {
		if err := m.catalog.Save(ctx, artifact.record(), source); err != nil {
			_ = module.Close(context.Background())
			return nil, m.persistenceFailure(err)
		}
	}
	m.artifacts[key] = artifact
	return artifact, nil
}

// Activate publishes one staged or retired revision. Existing snapshots retain
// their previous executable; future snapshots resolve the new revision.
func (m *Manager) Activate(artifact *Artifact) (*tool.Snapshot, error) {
	if artifact == nil || artifact.owner != m {
		return nil, ErrArtifactForeign
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.mutationError(); err != nil {
		return nil, err
	}
	key := artifactKey(artifact.manifest.Tool.Name, artifact.manifest.Tool.Revision)
	if m.artifacts[key] != artifact {
		return nil, ErrArtifactForeign
	}
	snapshot, err := m.registry.ActivateManyWithCommit(func(previous, _ *tool.Snapshot) error {
		if err := m.checkName(artifact.manifest, previous); err != nil {
			return err
		}
		return m.commitActive(artifact.manifest.Tool.Name, artifact.manifest.Tool.Revision)
	}, artifact.binding())
	if err != nil {
		return nil, err
	}
	if previous := m.activeByTool[artifact.manifest.Tool.Name]; previous != nil {
		previous.state = StateRetired
	}
	artifact.state = StateActive
	m.activeByTool[artifact.manifest.Tool.Name] = artifact
	return snapshot, nil
}

// Rollback atomically reactivates a retained revision.
func (m *Manager) Rollback(toolName, revision string) (*tool.Snapshot, error) {
	m.mu.Lock()
	artifact := m.artifacts[artifactKey(toolName, revision)]
	m.mu.Unlock()
	if artifact == nil {
		return nil, fmt.Errorf("%w: %s@%s", tool.ErrToolNotFound, toolName, revision)
	}
	return m.Activate(artifact)
}

// Close releases all compiled revisions. The application must stop accepting
// turns before calling it.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	artifacts := make([]*Artifact, 0, len(m.artifacts))
	for _, artifact := range m.artifacts {
		artifacts = append(artifacts, artifact)
	}
	m.mu.Unlock()

	var result error
	for _, artifact := range artifacts {
		result = errors.Join(result, artifact.module.Close(ctx))
	}
	return result
}

func artifactKey(toolName, revision string) string {
	return toolName + "@" + revision
}

func cloneDescriptor(descriptor tool.Descriptor) tool.Descriptor {
	descriptor.InputSchema = append([]byte(nil), descriptor.InputSchema...)
	descriptor.Capabilities = append([]string(nil), descriptor.Capabilities...)
	return descriptor
}

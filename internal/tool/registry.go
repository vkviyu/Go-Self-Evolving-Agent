// Package tool defines the native tool registry and execution boundary used by Eino.
package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"sort"
	"sync"
	"sync/atomic"
)

var toolNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

var (
	ErrToolNotFound    = errors.New("tool not found in registry snapshot")
	ErrInvalidTool     = errors.New("invalid tool descriptor")
	ErrRevisionExists  = errors.New("tool revision is already active")
	ErrForeignSnapshot = errors.New("registry snapshot belongs to a different registry")
)

// RiskClass groups tools for policy and approval defaults.
type RiskClass string

const (
	RiskPure    RiskClass = "pure"
	RiskRead    RiskClass = "read"
	RiskWrite   RiskClass = "write"
	RiskExecute RiskClass = "execute"
	RiskSecret  RiskClass = "secret"
)

// Descriptor is immutable model-facing tool metadata plus revision identity.
type Descriptor struct {
	Name         string
	Description  string
	InputSchema  json.RawMessage
	Revision     string
	RiskClass    RiskClass
	Capabilities []string
}

// Validate checks the stable registry invariants.
func (d Descriptor) Validate() error {
	if !toolNamePattern.MatchString(d.Name) {
		return fmt.Errorf("%w: name %q must match %s", ErrInvalidTool, d.Name, toolNamePattern)
	}
	if d.Description == "" {
		return fmt.Errorf("%w: description is required", ErrInvalidTool)
	}
	if d.Revision == "" {
		return fmt.Errorf("%w: revision is required", ErrInvalidTool)
	}
	if len(d.InputSchema) == 0 || !json.Valid(d.InputSchema) {
		return fmt.Errorf("%w: input schema must be valid JSON", ErrInvalidTool)
	}
	var root map[string]any
	if err := json.Unmarshal(d.InputSchema, &root); err != nil {
		return fmt.Errorf("%w: input schema must be an object: %v", ErrInvalidTool, err)
	}
	if schemaType, _ := root["type"].(string); schemaType != "object" {
		return fmt.Errorf("%w: input schema root type must be object", ErrInvalidTool)
	}
	switch d.RiskClass {
	case RiskPure, RiskRead, RiskWrite, RiskExecute, RiskSecret:
		return nil
	default:
		return fmt.Errorf("%w: unknown risk class %q", ErrInvalidTool, d.RiskClass)
	}
}

func cloneDescriptor(d Descriptor) Descriptor {
	d.InputSchema = append(json.RawMessage(nil), d.InputSchema...)
	d.Capabilities = append([]string(nil), d.Capabilities...)
	return d
}

// Executor executes a single normalized JSON tool call.
type Executor interface {
	Invoke(ctx context.Context, arguments json.RawMessage) (json.RawMessage, error)
}

// Binding couples immutable metadata to one executable revision.
type Binding struct {
	Descriptor       Descriptor
	Executor         Executor
	ImplementationID string
}

func (b Binding) validate() error {
	if err := b.Descriptor.Validate(); err != nil {
		return err
	}
	if b.Executor == nil {
		return fmt.Errorf("%w: executor is required", ErrInvalidTool)
	}
	if _, err := compileInputSchema(b.Descriptor.InputSchema); err != nil {
		return fmt.Errorf("%w: input schema: %w", ErrInvalidTool, err)
	}
	return nil
}

// Snapshot is an immutable view held for the duration of an Agent turn.
type Snapshot struct {
	generation   uint64
	bindings     map[string]Binding
	identityOnce sync.Once
	identity     string
	identityErr  error
	inputSchemas sync.Map // immutable schema cache, bounded by this snapshot's tools
}

// Generation is a process-local view identifier (published or reconstructed),
// not a durable identity or an individual tool revision.
func (s *Snapshot) Generation() uint64 {
	if s == nil {
		return 0
	}
	return s.generation
}

// Get resolves a binding without observing later registry updates.
func (s *Snapshot) Get(name string) (Binding, bool) {
	if s == nil {
		return Binding{}, false
	}
	binding, ok := s.bindings[name]
	if ok {
		binding.Descriptor = cloneDescriptor(binding.Descriptor)
	}
	return binding, ok
}

// Descriptors returns a stable, name-sorted metadata copy.
func (s *Snapshot) Descriptors() []Descriptor {
	if s == nil {
		return nil
	}
	names := make([]string, 0, len(s.bindings))
	for name := range s.bindings {
		names = append(names, name)
	}
	sort.Strings(names)
	descriptors := make([]Descriptor, 0, len(names))
	for _, name := range names {
		descriptors = append(descriptors, cloneDescriptor(s.bindings[name].Descriptor))
	}
	return descriptors
}

// Registry publishes copy-on-write snapshots. Existing turns retain their old view.
type Registry struct {
	mu            sync.Mutex
	current       atomic.Pointer[Snapshot]
	snapshots     map[uint64]*Snapshot
	generation    uint64
	snapshotStore SnapshotStore
}

// NewRegistry creates an empty generation-zero registry.
func NewRegistry() *Registry {
	registry := &Registry{snapshots: make(map[uint64]*Snapshot)}
	initial := &Snapshot{bindings: map[string]Binding{}}
	registry.current.Store(initial)
	registry.snapshots[initial.generation] = initial
	return registry
}

// Snapshot returns the current immutable registry view.
func (r *Registry) Snapshot() *Snapshot {
	return r.current.Load()
}

// SnapshotAt returns the immutable generation captured by a turn. Retired
// generations remain available so checkpoint resume cannot silently switch
// tool implementations.
func (r *Registry) SnapshotAt(generation uint64) (*Snapshot, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	snapshot, ok := r.snapshots[generation]
	return snapshot, ok
}

// OwnsSnapshot reports whether snapshot is an immutable generation published by
// this registry. Pipeline callers may retain a snapshot for a turn, but cannot
// inject a snapshot created by another registry instance.
func (r *Registry) OwnsSnapshot(snapshot *Snapshot) bool {
	if r == nil || snapshot == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	known, ok := r.snapshots[snapshot.generation]
	return ok && known == snapshot
}

// Activate atomically publishes a binding as the active revision for its name.
func (r *Registry) Activate(binding Binding) (*Snapshot, error) {
	return r.ActivateMany(binding)
}

// ActivateMany publishes a set of bindings as one registry generation. This
// keeps the initial built-in tool set consistent for turns created during
// startup and avoids exposing a partially registered registry.
func (r *Registry) ActivateMany(bindingsToActivate ...Binding) (*Snapshot, error) {
	return r.ActivateManyWithCommit(nil, bindingsToActivate...)
}

// PublicationCommit lets the host check ownership before a new set of tools
// becomes visible. It runs under the registry lock. An error rejects the batch.
type PublicationCommit func(previous, next *Snapshot) error

func (r *Registry) ActivateManyWithCommit(commit PublicationCommit, bindingsToActivate ...Binding) (*Snapshot, error) {
	if len(bindingsToActivate) == 0 {
		return nil, errors.New("at least one tool binding is required")
	}
	validated := make([]Binding, len(bindingsToActivate))
	seen := make(map[string]struct{}, len(bindingsToActivate))
	for index, binding := range bindingsToActivate {
		if err := binding.validate(); err != nil {
			return nil, err
		}
		if _, exists := seen[binding.Descriptor.Name]; exists {
			return nil, fmt.Errorf("duplicate tool binding %q", binding.Descriptor.Name)
		}
		seen[binding.Descriptor.Name] = struct{}{}
		binding.Descriptor = cloneDescriptor(binding.Descriptor)
		validated[index] = binding
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	old := r.current.Load()
	for _, binding := range validated {
		if existing, ok := old.bindings[binding.Descriptor.Name]; ok && existing.Descriptor.Revision == binding.Descriptor.Revision {
			return nil, fmt.Errorf("%w: %s@%s", ErrRevisionExists, binding.Descriptor.Name, binding.Descriptor.Revision)
		}
	}
	bindings := make(map[string]Binding, len(old.bindings)+len(validated))
	maps.Copy(bindings, old.bindings)
	for _, binding := range validated {
		bindings[binding.Descriptor.Name] = binding
	}
	next := &Snapshot{generation: r.generation + 1, bindings: bindings}
	if commit != nil {
		if err := commit(old, next); err != nil {
			return nil, err
		}
	}
	r.snapshots[next.generation] = next
	r.generation = next.generation
	r.current.Store(next)
	return next, nil
}

// Retire removes an active binding only when the caller names its exact revision.
func (r *Registry) Retire(name, revision string) (*Snapshot, error) {
	return r.RetireMany(Binding{Descriptor: Descriptor{Name: name, Revision: revision}})
}

// RetireMany atomically removes a set of active revisions. All names must
// still point at the revisions supplied by the caller; otherwise no change is
// published. This keeps composition-root rollback from exposing a partially
// retired binding set to a newly created turn.
func (r *Registry) RetireMany(bindingsToRetire ...Binding) (*Snapshot, error) {
	if len(bindingsToRetire) == 0 {
		return nil, errors.New("at least one tool binding is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	old := r.current.Load()
	names := make(map[string]struct{}, len(bindingsToRetire))
	for _, binding := range bindingsToRetire {
		name, revision := binding.Descriptor.Name, binding.Descriptor.Revision
		if name == "" {
			return nil, fmt.Errorf("%w: tool name is required", ErrInvalidTool)
		}
		if _, duplicate := names[name]; duplicate {
			return nil, fmt.Errorf("duplicate tool retirement %q", name)
		}
		names[name] = struct{}{}
		existing, ok := old.bindings[name]
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrToolNotFound, name)
		}
		if existing.Descriptor.Revision != revision {
			return nil, fmt.Errorf("active revision mismatch for %s: got %s, active %s", name, revision, existing.Descriptor.Revision)
		}
	}
	bindings := make(map[string]Binding, len(old.bindings)-len(bindingsToRetire))
	for existingName, existingBinding := range old.bindings {
		if _, retire := names[existingName]; !retire {
			bindings[existingName] = existingBinding
		}
	}
	next := &Snapshot{generation: r.generation + 1, bindings: bindings}
	r.snapshots[next.generation] = next
	r.generation = next.generation
	r.current.Store(next)
	return next, nil
}

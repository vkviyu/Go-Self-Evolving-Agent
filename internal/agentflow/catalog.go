package agentflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"sort"
	"strings"
	"sync"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

const (
	ListToolName = "flow_list"
	RunToolName  = "flow_run"
)

type (
	Descriptor struct {
		Name        string `json:"name"`
		Revision    string `json:"revision"`
		Fingerprint string `json:"fingerprint"`
		Description string `json:"description"`
	}
	Registration struct {
		Definition  *Definition
		Description string
	}

	// Catalog is the host-owned set of safe, high-level Flows visible to Agents.
	// Agents select and invoke registered definitions; they never upload operator
	// graphs or executable callbacks.
	Catalog struct {
		runtime *Runtime
		mu      sync.RWMutex
		entries map[string]catalogEntry
	}

	catalogEntry struct {
		descriptor Descriptor
		definition *Definition
	}
)

func NewCatalog(runtime *Runtime) (*Catalog, error) {
	if runtime == nil {
		return nil, errors.New("Flow catalog requires a runtime")
	}
	return &Catalog{runtime: runtime, entries: make(map[string]catalogEntry)}, nil
}

// Register atomically publishes validated definitions. Existing names are
// immutable for the process lifetime so a running durable Flow cannot silently
// switch implementation.
func (catalog *Catalog) Register(registrations ...Registration) error {
	if catalog == nil {
		return errors.New("Flow catalog is nil")
	}
	pending := make(map[string]catalogEntry, len(registrations))
	for _, registration := range registrations {
		if registration.Definition == nil {
			return errors.New("Flow registration has a nil definition")
		}
		if err := registration.Definition.Validate(); err != nil {
			return err
		}
		fingerprint, err := registration.Definition.Fingerprint()
		if err != nil {
			return err
		}
		description := strings.TrimSpace(registration.Description)
		if description == "" {
			return fmt.Errorf("Flow %q requires a description", registration.Definition.Name)
		}
		name := registration.Definition.Name
		if _, exists := pending[name]; exists {
			return fmt.Errorf("duplicate Flow registration %q", name)
		}
		copyDefinition := &Definition{Name: name, Revision: registration.Definition.Revision, Stages: append([]Stage(nil), registration.Definition.Stages...)}
		pending[name] = catalogEntry{descriptor: Descriptor{Name: name, Revision: copyDefinition.Revision, Fingerprint: fingerprint, Description: description}, definition: copyDefinition}
	}
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	for name := range pending {
		if _, exists := catalog.entries[name]; exists {
			return fmt.Errorf("Flow %q is already registered", name)
		}
	}
	maps.Copy(catalog.entries, pending)
	return nil
}

func (catalog *Catalog) List() []Descriptor {
	if catalog == nil {
		return nil
	}
	catalog.mu.RLock()
	descriptors := make([]Descriptor, 0, len(catalog.entries))
	for _, entry := range catalog.entries {
		descriptors = append(descriptors, entry.descriptor)
	}
	catalog.mu.RUnlock()
	sort.Slice(descriptors, func(left, right int) bool { return descriptors[left].Name < descriptors[right].Name })
	return descriptors
}

func (catalog *Catalog) Run(ctx context.Context, name string, invocation Invocation) (Result, error) {
	if catalog == nil || catalog.runtime == nil {
		return Result{}, errors.New("Flow catalog is unavailable")
	}
	catalog.mu.RLock()
	entry, found := catalog.entries[name]
	catalog.mu.RUnlock()
	if !found {
		return Result{}, fmt.Errorf("Flow %q is not registered", name)
	}
	fingerprint, err := entry.definition.Fingerprint()
	if err != nil {
		return Result{}, err
	}
	if fingerprint != entry.descriptor.Fingerprint {
		return Result{}, fmt.Errorf("%w: registered Flow %s@%s was mutated after publication", ErrDefinitionMismatch, name, entry.descriptor.Revision)
	}
	return catalog.runtime.Run(ctx, entry.definition, invocation)
}

func CatalogBindings(catalog *Catalog) []tool.Binding {
	return []tool.Binding{
		{
			Descriptor: tool.Descriptor{
				Name: ListToolName, Description: "List host-registered composable Flows available to this Agent.",
				InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
				Revision:    "builtin-v2", RiskClass: tool.RiskRead, Capabilities: []string{"flow.list"},
			},
			Executor: tool.InvokeFunc(func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
				var request struct{}
				decoder := json.NewDecoder(bytes.NewReader(raw))
				decoder.DisallowUnknownFields()
				if err := decoder.Decode(&request); err != nil {
					return nil, fmt.Errorf("decode flow_list request: %w", err)
				}
				return json.Marshal(map[string]any{"flows": catalog.List()})
			}),
		},
		{
			Descriptor: tool.Descriptor{
				Name: RunToolName, Description: "Run one host-registered Flow through durable checkpoints and the current Tool Pipeline.",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"input":{},"key":{"type":"string"},"idempotency_key":{"type":"string"}},"required":["name","input","idempotency_key"],"additionalProperties":false}`),
				Revision:    "builtin-v2", RiskClass: tool.RiskExecute, Capabilities: []string{"flow.run"},
			},
			Executor: tool.InvokeFunc(func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
				var request struct {
					Name           string          `json:"name"`
					Input          json.RawMessage `json:"input"`
					Key            string          `json:"key"`
					IdempotencyKey string          `json:"idempotency_key"`
				}
				decoder := json.NewDecoder(bytes.NewReader(raw))
				decoder.DisallowUnknownFields()
				if err := decoder.Decode(&request); err != nil {
					return nil, fmt.Errorf("decode flow_run request: %w", err)
				}
				if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
					return nil, errors.New("flow_run request contains trailing data")
				}
				if strings.TrimSpace(request.Name) == "" || strings.TrimSpace(request.IdempotencyKey) == "" || len(request.Input) == 0 {
					return nil, errors.New("flow_run requires name, input and idempotency_key")
				}
				trusted, ok := tool.InvocationFromContext(ctx)
				if !ok || trusted.SessionID == "" {
					return nil, errors.New("flow_run requires a trusted Tool Pipeline invocation")
				}
				actor, ok := orchestration.ActorScopeFromContext(ctx)
				if !ok || actor.IsZero() {
					return nil, errors.New("flow_run requires an actor scope")
				}
				pipeline, ok := tool.PipelineFromContext(ctx)
				if !ok {
					return nil, errors.New("flow_run caller pipeline is unavailable")
				}
				result, err := catalog.Run(ctx, request.Name, Invocation{
					Actor: actor, SessionID: trusted.SessionID, IdempotencyKey: request.IdempotencyKey,
					Execution: trusted.Execution,
					Input:     Value{Key: request.Key, Data: append(json.RawMessage(nil), request.Input...)},
					Pipeline:  pipeline, RegistryGeneration: trusted.RegistryGeneration, RuntimeSnapshotID: trusted.RuntimeSnapshotID,
				})
				if err != nil {
					return nil, err
				}
				return json.Marshal(map[string]any{"flow_id": result.Run.ID, "state": result.Run.State, "outputs": result.Outputs})
			}),
		},
	}
}

package tool

import (
	"context"
	"encoding/json"
	"testing"
)

func TestRegistrySnapshotRetainsOldRevision(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	first := testBinding("rev-1", "one")
	if _, err := registry.Activate(first); err != nil {
		t.Fatal(err)
	}
	oldSnapshot := registry.Snapshot()

	second := testBinding("rev-2", "two")
	if _, err := registry.Activate(second); err != nil {
		t.Fatal(err)
	}
	newSnapshot := registry.Snapshot()

	oldBinding, ok := oldSnapshot.Get("echo")
	if !ok || oldBinding.Descriptor.Revision != "rev-1" {
		t.Fatalf("old snapshot changed: %#v", oldBinding.Descriptor)
	}
	newBinding, ok := newSnapshot.Get("echo")
	if !ok || newBinding.Descriptor.Revision != "rev-2" {
		t.Fatalf("new snapshot missing revision: %#v", newBinding.Descriptor)
	}
	if oldSnapshot.Generation() >= newSnapshot.Generation() {
		t.Fatal("registry generation did not advance")
	}
}

func TestSnapshotGetDoesNotExposeMutableDescriptorSlices(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	if _, err := registry.Activate(testBinding("rev-1", "one")); err != nil {
		t.Fatal(err)
	}
	snapshot := registry.Snapshot()
	first, ok := snapshot.Get("echo")
	if !ok {
		t.Fatal("missing echo binding")
	}
	first.Descriptor.InputSchema[0] = '!'
	second, ok := snapshot.Get("echo")
	if !ok || !json.Valid(second.Descriptor.InputSchema) {
		t.Fatal("snapshot descriptor was mutated through a returned binding")
	}
}

func testBinding(revision, response string) Binding {
	return Binding{
		Descriptor: Descriptor{
			Name:        "echo",
			Description: "Echo test input.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":true}`),
			Revision:    revision,
			RiskClass:   RiskPure,
		},
		Executor: InvokeFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
			return json.Marshal(map[string]string{"value": response})
		}),
	}
}

func TestRegistryRetireManyIsAtomic(t *testing.T) {
	t.Parallel()
	registry := NewRegistry()
	first := testBinding("first-v1", "first")
	first.Descriptor.Name = "first"
	second := testBinding("second-v1", "second")
	second.Descriptor.Name = "second"
	if _, err := registry.ActivateMany(first, second); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.RetireMany(
		Binding{Descriptor: Descriptor{Name: "first", Revision: "first-v1"}},
		Binding{Descriptor: Descriptor{Name: "second", Revision: "wrong"}},
	); err == nil {
		t.Fatal("retire with revision mismatch was accepted")
	}
	if snapshot := registry.Snapshot(); snapshot.Generation() != 1 {
		t.Fatalf("failed atomic retirement advanced generation to %d", snapshot.Generation())
	}
	if _, ok := registry.Snapshot().Get("first"); !ok {
		t.Fatal("failed atomic retirement removed first binding")
	}
	if _, ok := registry.Snapshot().Get("second"); !ok {
		t.Fatal("failed atomic retirement removed second binding")
	}
}

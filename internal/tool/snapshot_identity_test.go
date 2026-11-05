package tool

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func identityRegistry(t *testing.T, bindings ...Binding) *Registry {
	t.Helper()
	registry := NewRegistry()
	for _, binding := range bindings {
		if _, err := registry.Activate(binding); err != nil {
			t.Fatal(err)
		}
	}
	return registry
}

func requireSnapshotID(t *testing.T, registry *Registry) string {
	t.Helper()
	id, err := registry.Snapshot().ID()
	if err != nil || !strings.HasPrefix(id, "registry-sha256:") {
		t.Fatalf("identity=%q err=%v", id, err)
	}
	return id
}

func TestRegistryIdentityIgnoresPublicationOrderAndSchemaWhitespace(t *testing.T) {
	first := testBinding("v1", "first")
	first.Descriptor.Capabilities = []string{"b", "a"}
	second := testBinding("v1", "second")
	second.Descriptor.Name = "second"
	original := identityRegistry(t, first, second)
	first.Descriptor.InputSchema = json.RawMessage(`{ "additionalProperties": true, "type": "object" }`)
	first.Descriptor.Capabilities = []string{"a", "b"}
	previous := testBinding("v0", "old")
	restarted := identityRegistry(t, previous, second, first)
	identity := requireSnapshotID(t, original)
	if identity != requireSnapshotID(t, restarted) {
		t.Fatal("same content has different identity")
	}
	if _, err := restarted.SnapshotByID(identity); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.SnapshotByID("registry-2"); !errors.Is(err, ErrSnapshotUnavailable) {
		t.Fatalf("legacy generation accepted: %v", err)
	}
}

func TestRegistryIdentityRejectsRevisionAndArtifactDrift(t *testing.T) {
	binding := testBinding("v1", "value")
	binding.ImplementationID = "wasm:original:timeout:100"
	id := requireSnapshotID(t, identityRegistry(t, binding))
	for _, change := range []func(*Binding){
		func(b *Binding) { b.Descriptor.Revision = "v2" },
		func(b *Binding) { b.ImplementationID = "wasm:changed:timeout:100" },
		func(b *Binding) { b.Descriptor.RiskClass = RiskWrite },
	} {
		changed := binding
		change(&changed)
		registry := identityRegistry(t, changed)
		if requireSnapshotID(t, registry) == id {
			t.Fatal("changed implementation has same identity")
		}
		if _, err := registry.SnapshotByID(id); !errors.Is(err, ErrSnapshotUnavailable) {
			t.Fatalf("unavailable snapshot: %v", err)
		}
	}
}

func TestRegistryIdentitySurvivesProcessRestart(t *testing.T) {
	registry := identityRegistry(t, testBinding("stable-v1", "value"))
	id := requireSnapshotID(t, registry)
	if expected := os.Getenv("GSEA_TEST_REGISTRY_ID"); expected != "" {
		if expected != id {
			t.Fatalf("cross-process identity changed: %s != %s", id, expected)
		}
		return
	}
	command := exec.Command(os.Args[0], "-test.run=^TestRegistryIdentitySurvivesProcessRestart$")
	command.Env = append(os.Environ(), "GSEA_TEST_REGISTRY_ID="+id)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("restart: %v\n%s", err, output)
	}
}

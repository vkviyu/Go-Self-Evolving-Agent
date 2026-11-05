package agentprofile

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
)

func TestDurableSnapshotPreservesEmptyGrantAndRejectsTampering(t *testing.T) {
	profile := validProfile("worker")
	profile.Tools = []string{}
	encoded, err := Encode(profile)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(encoded, profile.Revision)
	if err != nil || decoded.Tools == nil || decoded.AllowsTool("read") {
		t.Fatalf("empty grant widened: %#v, %v", decoded, err)
	}
	var envelope durableSnapshot
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope.Profile.Tools = nil
	tampered, _ := json.Marshal(envelope)
	if _, err := Decode(tampered, profile.Revision); !errors.Is(err, ErrSnapshot) {
		t.Fatalf("tampered snapshot: %v", err)
	}
	if _, err := Decode(encoded, "different"); !errors.Is(err, ErrSnapshot) {
		t.Fatalf("revision mismatch: %v", err)
	}
	if _, err := Decode(nil, profile.Revision); !errors.Is(err, ErrSnapshot) {
		t.Fatalf("missing snapshot: %v", err)
	}
}

func TestFingerprintCanonicalizesToolOrderButNotNilGrant(t *testing.T) {
	a := validProfile("worker")
	a.Tools = []string{"write", "read"}
	a.DelegableTools = []string{"write", "read"}
	b := a.clone()
	slices.Reverse(b.Tools)
	slices.Reverse(b.DelegableTools)
	aID, _ := a.Fingerprint()
	bID, _ := b.Fingerprint()
	if aID != bID || a.Tools[0] != "write" {
		t.Fatal("fingerprint is order-dependent or mutated profile")
	}
	a.Tools, b.Tools = nil, []string{}
	aID, _ = a.Fingerprint()
	bID, _ = b.Fingerprint()
	if aID == bID {
		t.Fatal("nil and empty grants have the same identity")
	}
}

func TestDelegationCannotRegainAncestorOrChildPrivileges(t *testing.T) {
	parent := validProfile("parent")
	parent.Tools = []string{"read", "agent_spawn"}
	parent.DelegableTools = []string{"read", "write", "agent_spawn"}
	parent.CanSpawn = true
	child := validProfile("child")
	child.Tools = []string{"agent_spawn"}
	child.DelegableTools = []string{"read", "write", "agent_spawn"}
	child.CanSpawn, child.CanManageTasks, child.CanSendMessages = true, true, true
	constrained, err := ConstrainDelegation(parent, child)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(constrained.Tools, []string{"agent_spawn"}) || !slices.Equal(constrained.DelegableTools, constrained.Tools) {
		t.Fatalf("effective grants widened: %#v", constrained)
	}
	if constrained.CanManageTasks || constrained.CanSendMessages {
		t.Fatal("control-plane privileges widened")
	}
	grandchild, err := ConstrainDelegation(constrained, validProfile("grandchild"))
	if err != nil || grandchild.AllowsTool("read") || grandchild.AllowsTool("write") {
		t.Fatalf("grandchild regained privileges: %#v %v", grandchild, err)
	}
	parent.Tools = []string{}
	constrained, err = ConstrainDelegation(parent, child)
	if err != nil || constrained.Tools == nil || len(constrained.Tools) != 0 {
		t.Fatalf("deny-all not preserved: %#v %v", constrained, err)
	}
	parent.WorkspaceMode = WorkspaceSharedReadonly
	if _, err := ConstrainDelegation(parent, child); err == nil {
		t.Fatal("read-only parent delegated writable workspace")
	}
}

func TestProfileRegistryRetainsRevisionsAndRejectsRebindingAtomically(t *testing.T) {
	registry := NewRegistry()
	v1 := validProfile("worker")
	if _, err := registry.Activate(v1); err != nil {
		t.Fatal(err)
	}
	v2 := v1.clone()
	v2.Revision, v2.Instruction = "v2", "new instruction"
	if _, err := registry.Activate(v2); err != nil {
		t.Fatal(err)
	}
	old, ok := registry.Snapshot().GetRevision("v1")
	if !ok || old.Instruction != v1.Instruction {
		t.Fatal("old revision disappeared")
	}
	changed := v2.clone()
	changed.Tools = []string{}
	third := validProfile("third")
	third.Revision = "v3"
	if _, err := registry.Activate(third, changed); err == nil {
		t.Fatal("rebound immutable revision")
	}
	if snapshot := registry.Snapshot(); snapshot.Generation() != 2 || len(snapshot.Names()) != 1 {
		t.Fatal("failed batch mutated registry")
	}
	if _, err := NewRegistry().Activate(v1, validProfile("another-name")); err == nil {
		t.Fatal("ambiguous revision in same batch accepted")
	}
}

package agentprofile

import "testing"

func validProfile(name string) Profile {
	return Profile{
		Name: name, Revision: "v1", ModelProfile: "primary",
		ContextInheritance: InheritSummary, WorkspaceMode: WorkspaceSharedLease,
		Limits: Limits{MaxIterations: 10, MaxToolCalls: 20, MaxChildren: 2, MaxWallTime: 1},
	}
}

func TestRegistrySnapshotIsImmutable(t *testing.T) {
	t.Parallel()
	registry := NewRegistry()
	if generation, err := registry.Activate(validProfile("worker")); err != nil || generation != 1 {
		t.Fatalf("activate generation=%d err=%v", generation, err)
	}
	snapshot := registry.Snapshot()
	profile, ok := snapshot.Get("worker")
	if !ok {
		t.Fatal("worker profile missing")
	}
	profile.Tools = append(profile.Tools, "workspace_write")
	profileAgain, ok := snapshot.Get("worker")
	if !ok || len(profileAgain.Tools) != 0 {
		t.Fatalf("snapshot leaked mutable profile: %#v", profileAgain)
	}
	if names := snapshot.Names(); len(names) != 1 || names[0] != "worker" {
		t.Fatalf("unexpected profile names: %v", names)
	}
}

func TestRegistryRejectsInvalidProfileWithoutMutation(t *testing.T) {
	t.Parallel()
	registry := NewRegistry()
	if _, err := registry.Activate(validProfile("worker"), Profile{Name: "broken"}); err == nil {
		t.Fatal("invalid profile was accepted")
	}
	if snapshot := registry.Snapshot(); snapshot.Generation() != 0 || len(snapshot.Names()) != 0 {
		t.Fatalf("registry mutated after failed activation: generation=%d names=%v", snapshot.Generation(), snapshot.Names())
	}
}

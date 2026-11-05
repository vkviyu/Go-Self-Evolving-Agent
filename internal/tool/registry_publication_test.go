package tool

import (
	"errors"
	"testing"
)

func TestRegistryPublicationCommitIsAtomic(t *testing.T) {
	registry := NewRegistry()
	first := testBinding("v1", "old")
	old, err := registry.Activate(first)
	if err != nil {
		t.Fatal(err)
	}
	second := testBinding("v2", "new")
	additional := testBinding("v1", "other")
	additional.Descriptor.Name = "other"
	failure := errors.New("durable commit failed")
	called := false
	_, err = registry.ActivateManyWithCommit(func(previous, next *Snapshot) error {
		called = true
		if previous != old || registry.Snapshot() != old {
			t.Fatal("published before durable commit")
		}
		if b, ok := next.Get("echo"); !ok || b.Descriptor.Revision != "v2" {
			t.Fatal("incomplete candidate")
		}
		if _, ok := next.Get("other"); !ok {
			t.Fatal("missing second binding")
		}
		return failure
	}, second, additional)
	if !called || !errors.Is(err, failure) || registry.Snapshot() != old {
		t.Fatalf("failed commit changed snapshot: %v", err)
	}
	if _, exists := registry.SnapshotAt(old.Generation() + 1); exists {
		t.Fatal("failed candidate retained")
	}
	if _, err := registry.ActivateManyWithCommit(nil, second, additional); err != nil {
		t.Fatal(err)
	}
	active := registry.Snapshot()
	_, err = registry.RetireManyWithCommit(func(previous, next *Snapshot) error {
		if previous != active || len(next.Descriptors()) != 0 {
			t.Fatal("invalid retirement candidate")
		}
		return failure
	}, second, additional)
	if !errors.Is(err, failure) || registry.Snapshot() != active {
		t.Fatalf("failed retirement changed snapshot: %v", err)
	}
	if _, err := registry.RetireManyWithCommit(nil, second, additional); err != nil {
		t.Fatal(err)
	}
	if b, ok := old.Get("echo"); !ok || b.Descriptor.Revision != "v1" {
		t.Fatal("pinned snapshot changed")
	}
}

func TestRegistryCommitNotCalledForInvalidPublication(t *testing.T) {
	registry := NewRegistry()
	first := testBinding("v1", "one")
	if _, err := registry.Activate(first); err != nil {
		t.Fatal(err)
	}
	commit := func(_, _ *Snapshot) error { t.Fatal("invalid publication invoked commit"); return nil }
	if _, err := registry.ActivateManyWithCommit(commit, first); err == nil {
		t.Fatal("accepted same revision")
	}
	if _, err := registry.ActivateManyWithCommit(commit, testBinding("v2", "two"), Binding{}); err == nil {
		t.Fatal("accepted invalid binding")
	}
	if _, err := registry.RetireManyWithCommit(commit, testBinding("missing", "")); err == nil {
		t.Fatal("accepted mismatched retirement")
	}
}

package atestore

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/retention"
)

func TestNativeCollectionPreparedReceiptResourceIsolationAndReopen(t *testing.T) {
	f := newCoreFixture(t)
	s := f.store
	ctx := t.Context()
	resource := "runtime-a"
	plan := retention.CollectionPlan{Tool: "echo", Revision: "v1", ContentSHA256: "digest", State: "retired", ArtifactBytes: 8, ReclaimableBytes: 8}
	plan.ID = CollectionPlanID(resource, plan)
	if plan.ID == CollectionPlanID("runtime-b", plan) {
		t.Fatal("resource not bound into plan")
	}
	if _, err := s.PreparePluginCollection(ctx, resource, plan, false); err == nil {
		t.Fatal("confirmation missing")
	}
	if _, err := s.PreparePluginCollection(ctx, "runtime-b", plan, true); err == nil {
		t.Fatal("foreign resource accepted")
	}
	// Simulate a process stopping immediately after the native prepared receipt.
	prepared := collectionRecord{Resource: resource, Receipt: retention.CollectionReceipt{Plan: plan, CollectedAt: time.Now().UTC()}}
	key := collectionKey(resource, plan.ID)
	coreMust(t, s.authority.Update(ctx, func(tx *ateauthority.Tx) error { return createRecord(tx, "collection-receipt", key, prepared) }))
	if err := s.CompletePluginCollection(ctx, resource, prepared.Receipt); !errors.Is(err, retention.ErrPlanChanged) {
		t.Fatalf("unauthed cleanup finalized: %v", err)
	}
	f.reopen(t)
	authorized, err := s.PreparePluginCollection(ctx, resource, plan, true)
	coreMust(t, err)
	if !reflect.DeepEqual(authorized, prepared.Receipt) {
		t.Fatal("prepared identity replaced")
	}
	coreMust(t, s.authority.View(ctx, func(tx *ateauthority.Tx) error {
		tombstone, err := getRecord[collectionRecord](tx, "collection-resource-tombstone", key)
		if err == nil && (!tombstone.Authorized || tombstone.Resource != resource) {
			t.Fatal("missing local resource authorization")
		}
		return err
	}))
	if _, found, err := s.PluginCollectionReceipt(ctx, "runtime-b", plan.ID); err != nil || found {
		t.Fatalf("foreign receipt visible: %v %v", found, err)
	}
	authorized.BlobDeleted = true
	coreMust(t, s.CompletePluginCollection(ctx, resource, authorized))
	coreMust(t, s.CompletePluginCollection(ctx, resource, authorized))
	replayed, err := s.PreparePluginCollection(ctx, resource, plan, true)
	coreMust(t, err)
	if !reflect.DeepEqual(authorized, replayed) {
		t.Fatal("completed receipt changed")
	}
	authorized.BlobDeleted = false
	if err := s.CompletePluginCollection(ctx, resource, authorized); !errors.Is(err, retention.ErrPlanChanged) {
		t.Fatalf("outcome replaced: %v", err)
	}
}

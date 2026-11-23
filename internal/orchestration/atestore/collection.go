package atestore

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/retention"
)

type collectionRecord struct {
	Resource   string
	Receipt    retention.CollectionReceipt
	Authorized bool
	Complete   bool
}

// CollectionPlanID scopes an exact revision plan to a host resource. Neither
// this receipt nor its tombstone retires the revision in another runtime.
func CollectionPlanID(resource string, plan retention.CollectionPlan) string {
	raw, _ := json.Marshal([]string{resource, plan.Digest()})
	return retention.Digest(raw)
}

func collectionKey(resource, planID string) string {
	raw, _ := json.Marshal([]string{resource, planID})
	return retention.Digest(raw)
}

func (store *Store) PluginCollectionReceipt(ctx context.Context, resource, planID string) (retention.CollectionReceipt, bool, error) {
	var record collectionRecord
	var found bool
	err := store.authority.View(ctx, func(tx *ateauthority.Tx) (err error) {
		found, err = tx.Get(recordKey("collection-receipt", collectionKey(resource, planID)), &record)
		return err
	})
	if err == nil && found && (record.Resource != resource || record.Receipt.Plan.ID != planID) {
		return retention.CollectionReceipt{}, false, retention.ErrPlanChanged
	}
	return record.Receipt, found, err
}

// PreparePluginCollection is called under exclusive runtime resource ownership,
// after a fresh local plan. It persists intent, then a native resource tombstone
// and audit authorization in a separate transaction, before local deletion.
func (store *Store) PreparePluginCollection(ctx context.Context, resource string, plan retention.CollectionPlan, confirmed bool) (retention.CollectionReceipt, error) {
	if !confirmed || resource == "" || plan.Tool == "" || plan.Revision == "" || plan.ID != CollectionPlanID(resource, plan) {
		return retention.CollectionReceipt{}, errors.New("collection requires resource, exact reviewed plan and explicit confirmation")
	}
	if len(plan.Blockers) > 0 {
		return retention.CollectionReceipt{}, retention.ErrBlocked
	}
	key := collectionKey(resource, plan.ID)
	var record collectionRecord
	err := store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		found, err := tx.Get(recordKey("collection-receipt", key), &record)
		if err != nil {
			return err
		}
		if found {
			if record.Resource != resource || record.Receipt.Plan.ID != plan.ID {
				return retention.ErrPlanChanged
			}
			return nil
		}
		record = collectionRecord{Resource: resource, Receipt: retention.CollectionReceipt{Plan: plan, CollectedAt: time.Now().UTC()}}
		return createRecord(tx, "collection-receipt", key, record)
	})
	if err != nil {
		return retention.CollectionReceipt{}, err
	}
	if !record.Authorized {
		err = store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
			current, err := getRecord[collectionRecord](tx, "collection-receipt", key)
			if err != nil {
				return err
			}
			if current.Authorized {
				record = current
				return nil
			}
			current.Authorized = true
			if err := createRecord(tx, "collection-resource-tombstone", key, current); err != nil {
				return err
			}
			if err := putRecord(tx, "collection-receipt", key, current); err != nil {
				return err
			}
			record = current
			return nil
		})
	}
	if err != nil {
		return retention.CollectionReceipt{}, err
	}
	return record.Receipt, nil
}

// CompletePluginCollection records the actual local outcome after cleanup.
// A failure here is retried from the same resource tombstone, never by creating
// a new collection identity or pretending the earlier deletion was rolled back.
func (store *Store) CompletePluginCollection(ctx context.Context, resource string, receipt retention.CollectionReceipt) error {
	return store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		key := collectionKey(resource, receipt.Plan.ID)
		record, err := getRecord[collectionRecord](tx, "collection-receipt", key)
		if err != nil {
			return err
		}
		if !record.Authorized || record.Resource != resource || record.Receipt.Plan.Digest() != receipt.Plan.Digest() ||
			!record.Receipt.CollectedAt.Equal(receipt.CollectedAt) || (record.Complete && record.Receipt.BlobDeleted != receipt.BlobDeleted) {
			return retention.ErrPlanChanged
		}
		record.Complete, record.Receipt = true, receipt
		return putRecord(tx, "collection-receipt", key, record)
	})
}

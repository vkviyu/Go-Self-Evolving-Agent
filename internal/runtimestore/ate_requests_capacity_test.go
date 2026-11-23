package runtimestore

import (
	"errors"
	"fmt"
	"testing"

	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
)

func capacityRecord(t *testing.T, id string, body []byte) aterequest.Record {
	t.Helper()
	record, err := aterequest.NewRecord("store", api.CommitRequest{
		Space: "space", Principal: "principal", RequestID: model.RequestID(id),
		Method: "native/v1", Body: body,
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestATERequestsCountBudgetDoesNotCapBacklog(t *testing.T) {
	ctx := t.Context()
	db := newRetentionRuntime(t)
	store := db.ATERequests()
	first := capacityRecord(t, "000", []byte("body"))
	for i := 0; i <= aterequest.MaxListRecords; i++ {
		if err := store.Insert(ctx, capacityRecord(t, fmt.Sprintf("%03d", i), []byte("body"))); err != nil {
			t.Fatal(err)
		}
	}
	if started, err := store.BeginAttempt(ctx, first); err != nil || !started {
		t.Fatalf("begin attempt: %v %v", started, err)
	}
	if err := store.Insert(ctx, first); err != nil {
		t.Fatalf("idempotent insert beyond read budget: %v", err)
	}
	if records, err := aterequest.ListPending(ctx, store); !errors.Is(err, aterequest.ErrLimit) || records != nil {
		t.Fatalf("incomplete inventory accepted: %d %v", len(records), err)
	}
	page, err := store.List(ctx, aterequest.ListOptions{Limit: aterequest.MaxListRecords, MaxBytes: aterequest.MaxListBytes})
	if err != nil || len(page) != aterequest.MaxListRecords || !page[0].Attempted {
		t.Fatalf("bounded first page lost unknown: %d %v", len(page), err)
	}
	key := page[len(page)-1].Request.Key()
	page, err = store.List(ctx, aterequest.ListOptions{After: &key, Limit: aterequest.MaxListRecords, MaxBytes: aterequest.MaxListBytes})
	if err != nil || len(page) != 1 || page[0].Request.RequestID != model.RequestID(fmt.Sprintf("%03d", aterequest.MaxListRecords)) {
		t.Fatalf("overflow not recoverable through cursor: %+v %v", page, err)
	}
	result := api.CommitResult{Code: api.CodeRejected, RequestID: first.Request.RequestID}
	if err := store.DeleteRejected(ctx, first, result); err != nil {
		t.Fatal(err)
	}
	records, err := aterequest.ListPending(ctx, store)
	if err != nil || len(records) != aterequest.MaxListRecords {
		t.Fatalf("exact inventory boundary rejected: %d %v", len(records), err)
	}
	if err := store.Insert(ctx, first); !errors.Is(err, aterequest.ErrRejected) {
		t.Fatalf("terminal reservation lost beyond read budget: %v", err)
	}
}

func TestATERequestsByteBudgetDoesNotCapBacklog(t *testing.T) {
	ctx := t.Context()
	db := newRetentionRuntime(t)
	store := db.ATERequests()
	body := make([]byte, api.MaxCommandBody)
	var records []aterequest.Record
	total := 0
	for i := 0; total <= aterequest.MaxListBytes; i++ {
		record := capacityRecord(t, fmt.Sprintf("large-%d", i), body)
		raw, err := aterequest.EncodeRecord(record)
		if err != nil {
			t.Fatal(err)
		}
		total += len(raw)
		records = append(records, record)
		if err := store.Insert(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	if started, err := store.BeginAttempt(ctx, records[0]); err != nil || !started {
		t.Fatalf("begin large attempt: %v %v", started, err)
	}
	if records, err := aterequest.ListPending(ctx, store); !errors.Is(err, aterequest.ErrLimit) || records != nil {
		t.Fatalf("short byte-bounded page treated as complete: %d %v", len(records), err)
	}
	options := aterequest.ListOptions{Limit: aterequest.MaxListRecords, MaxBytes: aterequest.MaxListBytes}
	count, pages := 0, 0
	for {
		page, err := store.List(ctx, options)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		pages++
		size := 0
		for _, record := range page {
			if !aterequest.Equal(record, records[count]) || record.Attempted != (count == 0) {
				t.Fatal("pagination changed original envelope or attempt")
			}
			raw, err := aterequest.EncodeRecord(record)
			if err != nil {
				t.Fatal(err)
			}
			size += len(raw)
			count++
		}
		if size > options.MaxBytes {
			t.Fatalf("read exceeded encoded-byte bound: %d", size)
		}
		key := page[len(page)-1].Request.Key()
		options.After = &key
	}
	if count != len(records) || pages < 2 {
		t.Fatalf("backlog lost: read=%d saved=%d pages=%d", count, len(records), pages)
	}
	t.Logf("preserved %d records totaling %d encoded bytes across %d bounded pages", count, total, pages)
}

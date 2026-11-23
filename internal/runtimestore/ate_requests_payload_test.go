package runtimestore_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vkviyu/agent-temporal-engine/agentprofile"
	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	sessionate "github.com/vkviyu/go-self-evolving-agent/internal/session/atestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/testfixture"
)

type pausePayloadUpload struct {
	sessionate.Executor
	err error
}

func (executor pausePayloadUpload) Execute(ctx context.Context, request api.CommitRequest) (api.CommitResult, error) {
	if request.Method == api.MethodMutateState {
		return api.CommitResult{}, executor.err
	}
	return executor.Executor.Execute(ctx, request)
}

// The session adapter's other payload tests use an in-memory PendingStore.
// Exercise its paired preparation with real SQL persistence and native reopen.
func TestSQLiteLargeAppendPreparationAndRecovery(t *testing.T) {
	for _, test := range []struct {
		name    string
		backlog int
		payload int
	}{
		{"near-maximum-payload", 0, sessionate.MaxAppendBytes - 2000},
		{"exact-inventory-boundary", aterequest.MaxListRecords - 2, 1300 << 10},
		{"whole-plan-admission-refused", aterequest.MaxListRecords - 1, 1300 << 10},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "runtime.db")
			db, err := testfixture.Open(t, ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			var storeID model.StoreID
			if err := db.Process.WithClient(ctx, func(c *client.Client) error {
				snapshot, err := c.Snapshot(ctx)
				storeID = snapshot.Fence.Store
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < test.backlog; i++ {
				record, err := aterequest.NewRecord(storeID, api.CommitRequest{
					Space: "unrelated", Principal: "other", RequestID: model.RequestID(fmt.Sprintf("seed-%03d", i)),
					Method: "native/v1", Body: []byte("unrelated pending envelope"),
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := db.ATERequests().Insert(ctx, record); err != nil {
					t.Fatal(err)
				}
			}
			interrupted := errors.New("pause after paired SQL preparation")
			events, err := sessionate.New(sessionate.Options{
				Process: db.Process, Pending: db.ATERequests(), Space: "sessions", StoreID: storeID,
				NewExecutor: func(c *client.Client, id model.StoreID) (sessionate.Executor, error) {
					executor, err := aterequest.NewExecutor(c, db.ATERequests(), id)
					return pausePayloadUpload{Executor: executor, err: interrupted}, err
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			input := session.PendingEvent{Type: session.EventUserMessage, CorrelationID: "large",
				Payload: json.RawMessage(`{"content":"` + strings.Repeat("x", test.payload) + `"}`)}
			_, appendErr := events.Append(ctx, "large", 0, input)
			before, err := aterequest.ListPending(ctx, db.ATERequests())
			if err != nil {
				t.Fatal(err)
			}
			if test.backlog+2 > aterequest.MaxListRecords {
				if !errors.Is(appendErr, sessionate.ErrLimit) || len(before) != test.backlog {
					t.Fatalf("whole-plan admission left partial preparation: pending=%d err=%v", len(before), appendErr)
				}
				return
			}
			if !errors.Is(appendErr, interrupted) || len(before) != test.backlog+2 {
				t.Fatalf("paired preparation failed: pending=%d err=%v", len(before), appendErr)
			}
			chunks, size := 0, 0
			for _, record := range before {
				if record.Attempted {
					t.Fatal("preparation submitted before both envelopes were saved")
				}
				raw, err := aterequest.EncodeRecord(record)
				if err != nil {
					t.Fatal(err)
				}
				size += len(raw)
				if record.Request.Method == api.MethodApplyAgent {
					command, err := agentprofile.DecodeCommand(record.Request.Body)
					if err != nil || command.AppendSessionEvents == nil {
						t.Fatalf("saved append command: %v", err)
					}
					for _, event := range command.AppendSessionEvents.Events {
						chunks += len(event.PayloadRef.Chunks)
					}
				}
			}
			if chunks < 2 || size > aterequest.MaxListBytes {
				t.Fatalf("invalid multi-chunk preparation: chunks=%d bytes=%d", chunks, size)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = testfixture.Open(t, ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			after, err := aterequest.ListPending(ctx, db.ATERequests())
			if err != nil || len(after) != len(before) {
				t.Fatalf("reopen lost paired preparation: %d %v", len(after), err)
			}
			for i := range before {
				if !aterequest.Equal(before[i], after[i]) || after[i].Attempted {
					t.Fatal("reopen changed an original command")
				}
			}
			// Preparation used the explicit "sessions" partition; recovery must
			// address that same pending space, not the fixture default.
			recoverStore, err := sessionate.New(sessionate.Options{
				Process: db.Process, Pending: db.ATERequests(), Space: "sessions", StoreID: storeID,
			})
			if err != nil {
				t.Fatal(err)
			}
			recovered, err := recoverStore.Append(ctx, "large", 0, input)
			if err != nil || len(recovered) != 1 || !bytes.Equal(recovered[0].Payload, input.Payload) {
				t.Fatalf("recover multi-chunk append: %v", err)
			}
			loaded, err := recoverStore.Load(ctx, "large", 0, 10)
			if err != nil || len(loaded) != 1 || !bytes.Equal(loaded[0].Payload, input.Payload) {
				t.Fatalf("native payload readback: %v", err)
			}
			remaining, err := aterequest.ListPending(ctx, db.ATERequests())
			if err != nil || len(remaining) != test.backlog {
				t.Fatalf("cleanup changed unrelated backlog: %d %v", len(remaining), err)
			}
			t.Logf("recovered %d chunks with %d pending records (%d encoded bytes) at preparation", chunks, len(before), size)
		})
	}
}

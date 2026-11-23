package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateports"
	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	sessionate "github.com/vkviyu/go-self-evolving-agent/internal/session/atestore"
)

type startupRejectionPending struct {
	aterequest.PendingStore
	records []aterequest.Record
	fail    bool
}

func (p *startupRejectionPending) List(_ context.Context, options aterequest.ListOptions) ([]aterequest.Record, error) {
	var records []aterequest.Record
	for _, record := range p.records {
		if options.After == nil || record.Request.RequestID > options.After.RequestID {
			records = append(records, record)
		}
	}
	return records, nil
}

func (p *startupRejectionPending) Get(_ context.Context, key api.RequestKey) (aterequest.Record, bool, error) {
	for _, record := range p.records {
		if record.Request.Key() == key {
			return record, true, nil
		}
	}
	return aterequest.Record{}, false, nil
}

func (p *startupRejectionPending) DeleteRejected(_ context.Context, record aterequest.Record, result api.CommitResult) error {
	if err := aterequest.VerifyRejection(record, result); err != nil {
		return err
	}
	if p.fail {
		return errors.New("injected local cleanup failure")
	}
	p.records = p.records[1:]
	return nil
}

type startupLookupOnly struct {
	t       *testing.T
	lookups int
}

func (c *startupLookupOnly) Commit(context.Context, api.CommitRequest) (api.CommitResult, error) {
	c.t.Fatal("startup submitted a command")
	return api.CommitResult{}, nil
}

func (c *startupLookupOnly) LookupRequest(_ context.Context, request api.LookupRequestRequest) (api.LookupRequestResult, error) {
	if request.RequestID != "z-unattempted" {
		c.t.Fatal("startup looked up a locally proven rejection")
	}
	c.lookups++
	return api.LookupRequestResult{Code: api.CodeNotFoundAtFence, ObservedFence: model.CommitFence{
		Store: "store", CommitDigest: model.CommitDigest(strings.Repeat("0", 64)),
	}}, nil
}

func TestAuthorityStartupContinuesAfterSavedRejectionCleanup(t *testing.T) {
	record, err := aterequest.NewRecord("store", api.CommitRequest{
		Space: authoritySpace, Principal: "gsea", RequestID: "a-rejected",
		Method: api.MethodApplySpace, Body: []byte("invalid command"),
	})
	if err != nil {
		t.Fatal(err)
	}
	record.Attempted = true
	record.Rejection = &api.CommitResult{Code: api.CodeRejected, RequestID: record.Request.RequestID}
	request := record.Request
	request.RequestID = "z-unattempted"
	next, err := aterequest.NewRecord("store", request)
	if err != nil {
		t.Fatal(err)
	}
	pending := &startupRejectionPending{records: []aterequest.Record{record, next}, fail: true}
	client := &startupLookupOnly{t: t}
	executor, err := aterequest.NewExecutor(client, pending, "store")
	if err != nil {
		t.Fatal(err)
	}
	if err := recoverAuthorityRequests(t.Context(), executor, pending, "store"); !errors.Is(err, aterequest.ErrRejectionCleanup) ||
		len(pending.records) != 2 || client.lookups != 0 {
		t.Fatalf("startup swallowed cleanup failure: %v", err)
	}
	pending.fail = false
	if err := recoverAuthorityRequests(t.Context(), executor, pending, "store"); err != nil ||
		len(pending.records) != 1 || client.lookups != 1 || pending.records[0].Attempted {
		t.Fatalf("startup failed to continue after local cleanup: %v", err)
	}
}

// TestAuthorityHistorySurvivesFreshRuntime proves that committed history and
// semantic state remain readable with a new empty local runtime database.
func TestAuthorityHistorySurvivesFreshRuntime(t *testing.T) {
	ctx := t.Context()
	binary := filepath.Join(t.TempDir(), "ate")
	command := exec.CommandContext(ctx, "go", "build", "-o", binary, "github.com/vkviyu/agent-temporal-engine/cmd/ate")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build native ATE: %v\n%s", err, output)
	}
	cfg := config.Config{
		HomeDirectory: t.TempDir(), DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir(),
		ATE: config.ATEConfig{Enabled: true, Binary: binary, Directory: filepath.Join(t.TempDir(), "ate"), Timeout: 30 * time.Second},
	}
	cfg, err := cfg.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	open := func() (*authorityResources, *runtimestore.Database) {
		t.Helper()
		db, err := runtimestore.OpenRuntime(ctx, filepath.Join(t.TempDir(), "runtime.db"))
		if err != nil {
			t.Fatal(err)
		}
		a, err := openAuthority(ctx, cfg, db.ATERequests())
		if err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = a.process.Close()
			_ = db.Close()
		})
		return a, db
	}
	first, db := open()
	appended, err := first.sessions.Append(ctx, "authority-history", 0, session.PendingEvent{
		Type: session.EventSessionCreated, CorrelationID: "authority-turn",
		Payload: json.RawMessage(`{"content":"original native fact"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	original := json.RawMessage("{ \"decision\" : \"preserve spelling\" }")
	state := authorityState{records: first.records}
	if err := state.Put(ctx, "original-decision", original); err != nil {
		t.Fatal(err)
	}
	if err := state.Put(ctx, "original-decision", original); err != nil {
		t.Fatalf("identical decision retry failed: %v", err)
	}
	// Startup must not submit even an invalid unattempted generic command.
	// Only its actual execution owner may decide whether to send it.
	rejected, err := aterequest.NewRecord(first.storeID, api.CommitRequest{
		Space: authoritySpace, Principal: "gsea", RequestID: "invalid-saved-command",
		Method: api.MethodApplySpace, Body: []byte("invalid space command"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ATERequests().Insert(ctx, rejected); err != nil {
		t.Fatal(err)
	}
	absent := rejected
	absent.Request.RequestID = "attempted-generic-without-ack"
	absent, err = aterequest.NewRecord(first.storeID, absent.Request)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ATERequests().Insert(ctx, absent); err != nil {
		t.Fatal(err)
	}
	if started, err := db.ATERequests().BeginAttempt(ctx, absent); err != nil || !started {
		t.Fatalf("save prior attempt: %v", err)
	}
	if err := first.bound.WithClient(ctx, func(c *client.Client) error {
		body, err := api.EncodeStateBatch(api.StateBatch{Operations: []api.StateMutation{
			{Key: []byte("startup-ack-only"), Kind: api.MutationPut, Value: api.Value{Kind: api.ValueBytes, Data: []byte("original")}},
		}})
		if err != nil {
			return err
		}
		acknowledged, err := aterequest.NewRecord(first.storeID, api.CommitRequest{
			Space: authoritySpace, Principal: "gsea", RequestID: "generic-lost-reply",
			Method: api.MethodMutateState, Body: body,
		})
		if err != nil {
			return err
		}
		if err := db.ATERequests().Insert(ctx, acknowledged); err != nil {
			return err
		}
		if started, err := db.ATERequests().BeginAttempt(ctx, acknowledged); err != nil || !started {
			return errors.Join(err, errors.New("cannot begin original attempt"))
		}
		ack, err := c.Commit(ctx, acknowledged.Request)
		if err != nil || ack.Code != api.CodeDurableAck {
			return errors.Join(err, errors.New("native original commit failed"))
		}
		executor, err := aterequest.NewExecutor(c, db.ATERequests(), first.storeID)
		if err != nil {
			return err
		}
		return recoverAuthorityRequests(ctx, executor, db.ATERequests(), first.storeID)
	}); err != nil {
		t.Fatalf("unattempted command blocked startup: %v", err)
	}
	pending, err := db.ATERequests().List(ctx, aterequest.ListOptions{Limit: aterequest.MaxListRecords, MaxBytes: aterequest.MaxListBytes})
	if err != nil || len(pending) != 2 || !pending[0].Attempted || !aterequest.Equal(pending[0], absent) ||
		pending[1].Attempted || !aterequest.Equal(pending[1], rejected) {
		t.Fatalf("startup changed an unattempted command: %d, %v", len(pending), err)
	}
	if err := first.process.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	second, _ := open()
	got, err := second.sessions.Load(ctx, "authority-history", 0, 100)
	if err != nil || len(got) != 1 {
		t.Fatalf("read native history: count=%d err=%v", len(got), err)
	}
	if got[0].EventID != appended[0].EventID || got[0].Checksum != appended[0].Checksum {
		t.Fatal("history identity changed after replacing local runtime")
	}
	secondState := authorityState{records: second.records}
	value, found, err := secondState.Get(ctx, "original-decision")
	if err != nil || !found || !bytes.Equal(value, original) {
		t.Fatalf("native decision state changed: found=%v err=%v", found, err)
	}
	if err := secondState.CompareAndSwap(ctx, "original-decision", original, json.RawMessage(`{"decision":"revised"}`)); err != nil {
		t.Fatal(err)
	}
	if err := secondState.CompareAndSwap(ctx, "original-decision", original, original); !errors.Is(err, ateports.ErrConflict) {
		t.Fatalf("stale semantic CAS accepted: %v", err)
	}
}

type startupPending struct {
	*runtimestore.ATERequestStore
	mode     string
	original session.Event
}

type startupTerminalPending struct {
	aterequest.PendingStore
	record  aterequest.Record
	fail    bool
	deleted bool
}

func (p *startupTerminalPending) Get(context.Context, api.RequestKey) (aterequest.Record, bool, error) {
	return p.record, !p.deleted, nil
}

func (p *startupTerminalPending) List(_ context.Context, options aterequest.ListOptions) ([]aterequest.Record, error) {
	if p.deleted || options.After != nil {
		return nil, nil
	}
	return []aterequest.Record{p.record}, nil
}

func (p *startupTerminalPending) DeleteRejected(_ context.Context, record aterequest.Record, result api.CommitResult) error {
	if err := aterequest.VerifyRejection(record, result); err != nil {
		return err
	}
	if p.fail {
		return errors.New("injected local cleanup failure")
	}
	p.deleted = true
	return nil
}

func TestAuthorityStartupFinishesSavedGenericRejection(t *testing.T) {
	record, err := aterequest.NewRecord("store", api.CommitRequest{
		Space: authoritySpace, Principal: "gsea", RequestID: "generic-rejected",
		Method: api.MethodApplySpace, Body: []byte("invalid command"),
	})
	if err != nil {
		t.Fatal(err)
	}
	record.Attempted = true
	record.Rejection = &api.CommitResult{Code: api.CodeRejected, RequestID: record.Request.RequestID}
	pending := &startupTerminalPending{record: record, fail: true}
	// The disconnected client cannot provide an ACK. The persisted rejection
	// alone must permit local cleanup, while cleanup errors remain visible.
	executor, err := aterequest.NewExecutor(&startupLostReply{lost: true}, pending, record.StoreID)
	if err != nil {
		t.Fatal(err)
	}
	if err := recoverAuthorityRequests(t.Context(), executor, pending, record.StoreID); !errors.Is(err, aterequest.ErrRejectionCleanup) || pending.deleted {
		t.Fatalf("startup hid failed rejection cleanup: %v", err)
	}
	pending.fail = false
	if err := recoverAuthorityRequests(t.Context(), executor, pending, record.StoreID); err != nil || !pending.deleted {
		t.Fatalf("cleaned rejection blocked startup or remained pending: %v", err)
	}
}

func (p *startupPending) Insert(ctx context.Context, record aterequest.Record) error {
	if strings.HasPrefix(string(record.Request.RequestID), "gsea-session-upload-") {
		batch, err := api.DecodeStateBatch(record.Request.Body)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(batch.Operations[0].Value.Data, &p.original); err != nil {
			return err
		}
	}
	if strings.HasPrefix(p.mode, "pre-insert") && strings.HasPrefix(string(record.Request.RequestID), "gsea-session-append-") {
		return errors.New("injected crash before append insert")
	}
	return p.ATERequestStore.Insert(ctx, record)
}

func (p *startupPending) BeginAttempt(ctx context.Context, record aterequest.Record) (bool, error) {
	if p.mode == "unattempted" && strings.HasPrefix(string(record.Request.RequestID), "gsea-session-upload-") {
		return false, errors.New("injected stop before upload attempt")
	}
	return p.ATERequestStore.BeginAttempt(ctx, record)
}

func (p *startupPending) RetireUnattempted(ctx context.Context, record aterequest.Record) error {
	if strings.HasPrefix(p.mode, "reject-") {
		return errors.New("injected crash before dependent retirement")
	}
	return p.ATERequestStore.RetireUnattempted(ctx, record)
}

type startupLostReply struct {
	aterequest.Client
	mode string
	lost bool
}

func (c *startupLostReply) LookupRequest(ctx context.Context, request api.LookupRequestRequest) (api.LookupRequestResult, error) {
	if c.lost {
		return api.LookupRequestResult{}, io.ErrUnexpectedEOF
	}
	return c.Client.LookupRequest(ctx, request)
}

func (c *startupLostReply) Commit(ctx context.Context, request api.CommitRequest) (api.CommitResult, error) {
	upload := strings.HasPrefix(string(request.RequestID), "gsea-session-upload-")
	appendRequest := strings.HasPrefix(string(request.RequestID), "gsea-session-append-")
	if strings.HasPrefix(c.mode, "reject-") && upload {
		return api.CommitResult{Code: api.CodeRejected, RequestID: request.RequestID}, nil
	}
	target := upload && strings.HasPrefix(c.mode, "upload-") || appendRequest && strings.HasPrefix(c.mode, "append-")
	if !target {
		return c.Client.Commit(ctx, request)
	}
	if strings.HasSuffix(c.mode, "-after") {
		result, err := c.Client.Commit(ctx, request)
		if err != nil || result.Code != api.CodeDurableAck {
			return result, err
		}
	}
	c.lost = true
	return api.CommitResult{Code: api.CodeCommitUnknown, RequestID: request.RequestID}, io.ErrUnexpectedEOF
}

// Uses a built ATE binary, real stdio service and reopened SQLite pending store.
// Faults occur around the trusted client's actual submission and local inserts.
func TestAuthorityStartupSessionPlans(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "ate")
	if output, err := exec.CommandContext(t.Context(), "go", "build", "-o", binary,
		"github.com/vkviyu/agent-temporal-engine/cmd/ate").CombinedOutput(); err != nil {
		t.Fatalf("build ATE: %v\n%s", err, output)
	}
	for _, mode := range []string{"pre-insert", "pre-insert-acked", "unattempted", "offline", "upload-after", "append-after", "upload-before", "append-before", "reject-gap", "reject-attempted"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			cfg, err := (config.Config{
				HomeDirectory: t.TempDir(), DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir(),
				ATE: config.ATEConfig{Enabled: true, Binary: binary, Directory: filepath.Join(t.TempDir(), "ate"), Timeout: 30 * time.Second},
			}).Resolve()
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "runtime.db")
			db, err := runtimestore.OpenRuntime(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			first, err := openAuthority(ctx, cfg, db.ATERequests())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = first.process.Close() })
			pending := &startupPending{ATERequestStore: db.ATERequests(), mode: mode}
			if mode == "offline" {
				pending.mode = "unattempted"
			}
			writer, err := sessionate.New(sessionate.Options{
				Process: first.bound, Pending: pending, Space: authoritySpace, StoreID: first.storeID,
				NewExecutor: func(c *client.Client, id model.StoreID) (sessionate.Executor, error) {
					return aterequest.NewExecutor(&startupLostReply{Client: c, mode: mode}, pending, id)
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			input := session.PendingEvent{Type: session.EventUserMessage, CorrelationID: "original-turn",
				Payload: json.RawMessage(`{"text":"` + strings.Repeat("x", 400<<10) + `"}`)}
			if _, err := writer.Append(ctx, "startup-session", 0, input); err == nil {
				t.Fatal("fault did not interrupt append")
			}
			original := pending.original
			list := func() []aterequest.Record {
				t.Helper()
				records, err := db.ATERequests().List(ctx, aterequest.ListOptions{Limit: 128, MaxBytes: aterequest.MaxListBytes})
				if err != nil {
					t.Fatal(err)
				}
				return records
			}
			before := list()
			if len(before) == 0 || original.EventID == "" {
				t.Fatal("recovery material missing")
			}
			if mode == "reject-attempted" {
				if started, err := db.ATERequests().BeginAttempt(ctx, before[0]); err != nil || !started {
					t.Fatalf("prepare legacy out-of-order append: %v", err)
				}
				before = list()
			}
			if mode == "pre-insert-acked" {
				// Reproduce an older generic startup having submitted the
				// sole saved upload before the final Insert ever happened.
				record := before[0]
				if started, err := db.ATERequests().BeginAttempt(ctx, record); err != nil || !started {
					t.Fatalf("prepare upload attempt: %v", err)
				}
				if err := first.bound.WithClient(ctx, func(c *client.Client) error {
					result, err := c.Commit(ctx, record.Request)
					if err != nil {
						return err
					}
					return aterequest.VerifyAcknowledgement(record, result)
				}); err != nil {
					t.Fatal(err)
				}
				before = list()
			}
			if err := first.process.Close(); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = runtimestore.OpenRuntime(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			var startupStore authorityPendingStore = db.ATERequests()
			if mode == "offline" {
				holder := &App{config: cfg}
				defer func() {
					for _, lock := range holder.sessionOwners {
						_ = lock.Close()
					}
				}()
				blocked := make(map[api.RequestKey]bool)
				for _, record := range before {
					blocked[record.Request.Key()] = true
				}
				startupStore = &offlineSessionPending{authorityPendingStore: db.ATERequests(),
					owner: holder, id: "startup-session", preexisting: blocked}
			}
			second, err := openAuthority(ctx, cfg, startupStore)
			if err != nil {
				t.Fatalf("startup blocked on scoped pending: %v", err)
			}
			defer second.process.Close()
			if mode == "offline" {
				loaded, err := second.sessions.Load(ctx, "startup-session", 0, 10)
				if err != nil || len(loaded) != 1 || !reflect.DeepEqual(loaded[0], original) || len(list()) != 0 {
					t.Fatalf("offline owned startup did not recover exactly: count=%d err=%v", len(loaded), err)
				}
				return
			}
			if after := list(); !reflect.DeepEqual(before, after) {
				t.Fatal("startup sent or removed session recovery material before ownership")
			}
			owner := &App{config: cfg, authority: second}
			defer func() {
				for _, lock := range owner.sessionOwners {
					_ = lock.Close()
				}
			}()
			if err := owner.RecoverSessionRequests(ctx, "startup-session"); err == nil {
				t.Fatal("recovery accepted absent execution ownership")
			}
			owner.stateMu.Lock()
			err = owner.ownSession("unrelated-session")
			owner.stateMu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if err := owner.RecoverSessionRequests(ctx, "unrelated-session"); err != nil || !reflect.DeepEqual(before, list()) {
				t.Fatalf("another Session's owner changed the plan: %v", err)
			}
			owner.stateMu.Lock()
			err = owner.ownSession("startup-session")
			owner.stateMu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			err = owner.RecoverSessionRequests(ctx, "startup-session")
			if strings.HasSuffix(mode, "-before") || mode == "reject-attempted" {
				if !errors.Is(err, aterequest.ErrUnknown) || errors.Is(err, aterequest.ErrRejected) || !reflect.DeepEqual(before, list()) {
					t.Fatalf("attempted absence was replayed or discarded: %v", err)
				}
				return
			}
			if mode == "reject-gap" {
				if !errors.Is(err, aterequest.ErrRejected) || len(list()) != 0 {
					t.Fatalf("rejected upload left its dependent stuck: %v", err)
				}
				// The rejected identity is fenced: identical bytes are the same
				// ATE request. New work must name a different event.
				if _, err := second.sessions.Append(ctx, "startup-session", 0, input); err == nil ||
					(!errors.Is(err, aterequest.ErrRejected) && !errors.Is(err, aterequest.ErrConflict)) {
					t.Fatalf("rejected identity was not fenced: %v", err)
				}
				fresh := input
				fresh.Payload = json.RawMessage(`{"content":"explicit new work"}`)
				if _, err := second.sessions.Append(ctx, "startup-session", 0, fresh); err != nil {
					t.Fatalf("explicit new work was blocked: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("owned recovery: %v", err)
			}
			loaded, err := second.sessions.Load(ctx, "startup-session", 0, 10)
			if err != nil || len(loaded) != 1 || !reflect.DeepEqual(loaded[0], original) || len(list()) != 0 {
				t.Fatalf("original append not published exactly once: count=%d err=%v", len(loaded), err)
			}
			if err := owner.RecoverSessionRequests(ctx, "startup-session"); err != nil {
				t.Fatalf("completed recovery not idempotent: %v", err)
			}
		})
	}
}

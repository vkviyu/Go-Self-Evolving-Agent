package runtimestore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/engine"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/agent-temporal-engine/protocol"
	"github.com/vkviyu/agent-temporal-engine/storage"
	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
)

type (
	// disconnectedATE can lose the request before submission or the reply after
	// durable commit. Recovery uses a new client and the reopened authorities.
	disconnectedATE struct {
		aterequest.Client
		lose         bool
		beforeCommit bool
		disconnected bool
		commits      [][]byte
	}

	terminalATE struct {
		result  api.CommitResult
		err     error
		accept  bool
		commits int
	}

	classifiedATE struct {
		aterequest.Client
	}
)

func (client *classifiedATE) ClassifyError(err error) api.ErrorClass {
	return engine.ClassifyError(err)
}

func (client *terminalATE) LookupRequest(context.Context, api.LookupRequestRequest) (api.LookupRequestResult, error) {
	return api.LookupRequestResult{Code: api.CodeNotFoundAtFence, ObservedFence: model.CommitFence{
		Store: "store", CommitDigest: model.CommitDigest(strings.Repeat("0", 64)),
	}}, nil
}

func (client *terminalATE) Commit(_ context.Context, request api.CommitRequest) (api.CommitResult, error) {
	client.commits++
	if client.accept {
		record, err := aterequest.NewRecord("store", request)
		if err != nil {
			return api.CommitResult{}, err
		}
		return atePendingAck(record), nil
	}
	return client.result, client.err
}

func (client *disconnectedATE) Commit(ctx context.Context, request api.CommitRequest) (api.CommitResult, error) {
	raw, err := api.CanonicalCommand(request)
	if err != nil {
		return api.CommitResult{}, err
	}
	client.commits = append(client.commits, raw)
	if !client.lose {
		return client.Client.Commit(ctx, request)
	}
	if !client.beforeCommit {
		if result, err := client.Client.Commit(ctx, request); err != nil || result.Code != api.CodeDurableAck {
			return result, err
		}
	}
	client.disconnected = true
	return api.CommitResult{Code: api.CodeCommitUnknown, RequestID: request.RequestID}, io.ErrUnexpectedEOF
}

func (client *disconnectedATE) LookupRequest(ctx context.Context, key api.LookupRequestRequest) (api.LookupRequestResult, error) {
	if client.disconnected {
		return api.LookupRequestResult{Code: api.CodeIndeterminate}, io.ErrUnexpectedEOF
	}
	return client.Client.LookupRequest(ctx, key)
}

func atePendingRecord(t *testing.T, id string) aterequest.Record {
	t.Helper()
	record, err := aterequest.NewRecord("store", api.CommitRequest{
		Space: "space", Principal: "principal", RequestID: model.RequestID(id),
		Method: "native/v1", Body: []byte{0, 0xff, '\n', 0x80},
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestATERequestRetirementProvesNeverAttempted(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "runtime.db")
	database, err := OpenRuntime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	store := database.ATERequests()
	record := atePendingRecord(t, "retired")
	if err := store.Insert(ctx, record); err != nil {
		t.Fatal(err)
	}
	changed := record
	changed.Request.Body = []byte("different")
	changed, _ = aterequest.NewRecord(record.StoreID, changed.Request)
	if err := store.RetireUnattempted(ctx, changed); !errors.Is(err, aterequest.ErrConflict) {
		t.Fatalf("changed envelope retired: %v", err)
	}
	if err := store.RetireUnattempted(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = OpenRuntime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store = database.ATERequests()
	if err := store.Insert(ctx, record); !errors.Is(err, aterequest.ErrRetired) {
		t.Fatalf("retired identity reinserted: %v", err)
	}
	terminal, found, err := store.GetTerminal(ctx, record.Request.Key())
	if err != nil || !found || !terminal.Retired || terminal.Result != (api.CommitResult{}) {
		t.Fatalf("retirement fabricated native receipt: %+v %v", terminal, err)
	}
	attempted := atePendingRecord(t, "attempted")
	if err := store.Insert(ctx, attempted); err != nil {
		t.Fatal(err)
	}
	if started, err := store.BeginAttempt(ctx, attempted); err != nil || !started {
		t.Fatalf("attempt failed: %v", err)
	}
	if err := store.RetireUnattempted(ctx, attempted); !errors.Is(err, aterequest.ErrConflict) {
		t.Fatalf("attempted unknown retired: %v", err)
	}
	if got, found, err := store.Get(ctx, attempted.Request.Key()); err != nil || !found || !got.Attempted {
		t.Fatalf("unknown was removed: %v", err)
	}
}

func TestATERequestRetirementRacesAttempt(t *testing.T) {
	ctx := t.Context()
	database, err := OpenRuntime(ctx, filepath.Join(t.TempDir(), "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := database.ATERequests()
	for i := 0; i < 24; i++ {
		record := atePendingRecord(t, fmt.Sprintf("race-%d", i))
		if err := store.Insert(ctx, record); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		attempt := make(chan bool, 1)
		retired := make(chan error, 1)
		go func() {
			<-start
			started, _ := store.BeginAttempt(ctx, record)
			attempt <- started
		}()
		go func() {
			<-start
			retired <- store.RetireUnattempted(ctx, record)
		}()
		close(start)
		started, retirementErr := <-attempt, <-retired
		if started == (retirementErr == nil) {
			t.Fatalf("attempt and retirement did not exclude each other: started=%v err=%v", started, retirementErr)
		}
		got, found, err := store.Get(ctx, record.Request.Key())
		if err != nil || found != started || found && !got.Attempted {
			t.Fatalf("race lost attempt proof: found=%v attempted=%v err=%v", found, got.Attempted, err)
		}
	}
}

func TestATERequestRetirementRollsBackAsOneUnit(t *testing.T) {
	ctx := t.Context()
	database, err := OpenRuntime(ctx, filepath.Join(t.TempDir(), "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := database.ATERequests()
	record := atePendingRecord(t, "retire-rollback")
	if err := store.Insert(ctx, record); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, `CREATE TRIGGER fail_retirement
BEFORE DELETE ON ate_pending_requests BEGIN SELECT RAISE(ABORT, 'injected retirement failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.RetireUnattempted(ctx, record); err == nil {
		t.Fatal("retirement ignored payload deletion failure")
	}
	if _, found, err := store.GetTerminal(ctx, record.Request.Key()); err != nil || found {
		t.Fatalf("failed retirement left a terminal fence: found=%v err=%v", found, err)
	}
	got, found, err := store.Get(ctx, record.Request.Key())
	if err != nil || !found || got.Attempted || !aterequest.Equal(got, record) {
		t.Fatalf("failed retirement damaged original request: found=%v err=%v", found, err)
	}
	if started, err := store.BeginAttempt(ctx, record); err != nil || !started {
		t.Fatalf("rolled-back retirement blocked its original request: %v", err)
	}
}

func atePendingAck(record aterequest.Record) api.CommitResult {
	return api.CommitResult{
		Code: api.CodeDurableAck, RequestID: record.Request.RequestID, RequestDigest: record.Digest,
		Fence: model.CommitFence{Store: record.StoreID, LSN: 1, CommitDigest: model.CommitDigest(strings.Repeat("a", 64))},
	}
}

func TestATERequestsScopeAndMemoryIsolation(t *testing.T) {
	ctx := t.Context()
	db := openATEPendingDB(t, ":memory:")
	store := db.ATERequests()
	var records []aterequest.Record
	for _, key := range []api.RequestKey{
		{Space: "a", Principal: "a", RequestID: "same"},
		{Space: "a", Principal: "b", RequestID: "same"},
		{Space: "b", Principal: "a", RequestID: "same"},
	} {
		request := atePendingRecord(t, "same").Request
		request.Space, request.Principal = key.Space, key.Principal
		request.ExpectedFence = &model.CommitFence{Store: "store", CommitDigest: model.CommitDigest(strings.Repeat("0", 64))}
		record, err := aterequest.NewRecord("store", request)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Insert(ctx, record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	page, err := store.List(ctx, aterequest.ListOptions{Limit: 10, MaxBytes: 1 << 20})
	if err != nil || len(page) != 3 {
		t.Fatalf("scoped list: %d %v", len(page), err)
	}
	for i, record := range records {
		if !aterequest.Equal(page[i], record) {
			t.Fatal("scope ordering changed")
		}
	}
	page[0].Request.Body[0] ^= 0xff
	page[0].Request.ExpectedFence.Store = "mutated"
	got, found, err := store.Get(ctx, records[0].Request.Key())
	if err != nil || !found || !aterequest.Equal(got, records[0]) {
		t.Fatalf("returned data aliases saved envelope: %v", err)
	}
	key := records[1].Request.Key()
	page, err = store.List(ctx, aterequest.ListOptions{After: &key, Limit: 10, MaxBytes: 1 << 20})
	if err != nil || len(page) != 1 || !aterequest.Equal(page[0], records[2]) {
		t.Fatalf("scoped cursor: %+v %v", page, err)
	}
	if err := store.RetireUnattempted(ctx, records[1]); err != nil {
		t.Fatal(err)
	}
	for _, record := range []aterequest.Record{records[0], records[2]} {
		if got, found, err := store.Get(ctx, record.Request.Key()); err != nil || !found || !aterequest.Equal(got, record) {
			t.Fatalf("retirement crossed scope: %v", err)
		}
	}
	if err := store.BindAuthority(ctx, "other"); !errors.Is(err, aterequest.ErrConflict) {
		t.Fatalf("existing envelopes rebound: %v", err)
	}
	if err := store.BindAuthority(ctx, "store"); err != nil {
		t.Fatal(err)
	}
	foreign := atePendingRecord(t, "foreign")
	foreign.StoreID = "other"
	if err := store.Insert(ctx, foreign); !errors.Is(err, aterequest.ErrConflict) {
		t.Fatalf("foreign authority inserted: %v", err)
	}
	if err := store.RetireUnattempted(ctx, foreign); !errors.Is(err, aterequest.ErrConflict) {
		t.Fatalf("foreign authority retired: %v", err)
	}
}

func TestATERequestsAttemptsSerializeAcrossHandles(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "runtime.db")
	left, right := openATEPendingDB(t, path).ATERequests(), openATEPendingDB(t, path).ATERequests()
	record := atePendingRecord(t, "race")
	if err := left.Insert(ctx, record); err != nil {
		t.Fatal(err)
	}
	type attempt struct {
		started bool
		err     error
	}
	start := make(chan struct{})
	results := make(chan attempt, 16)
	for i := 0; i < cap(results); i++ {
		store := left
		if i%2 != 0 {
			store = right
		}
		go func() {
			<-start
			started, err := store.BeginAttempt(ctx, record)
			results <- attempt{started, err}
		}()
	}
	close(start)
	winners := 0
	for i := 0; i < cap(results); i++ {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.started {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("attempt acquired %d times", winners)
	}
	if err := right.RetireUnattempted(ctx, record); !errors.Is(err, aterequest.ErrConflict) {
		t.Fatalf("second handle retired attempted unknown: %v", err)
	}
}

func TestATERequestsMissingRetirementAndTerminalCorruption(t *testing.T) {
	ctx := t.Context()
	db := openATEPendingDB(t, ":memory:")
	store := db.ATERequests()
	record := atePendingRecord(t, "never-inserted")
	for range 2 {
		if err := store.RetireUnattempted(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	if started, err := store.BeginAttempt(ctx, record); started || !errors.Is(err, aterequest.ErrRetired) {
		t.Fatalf("retired key attempted: %v %v", started, err)
	}
	if err := store.DeleteAcknowledged(ctx, record, atePendingAck(record)); !errors.Is(err, aterequest.ErrConflict) {
		t.Fatalf("retirement accepted acknowledgement: %v", err)
	}
	if _, err := db.db.Exec(`UPDATE ate_rejected_requests SET checksum='corrupt'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.GetTerminal(ctx, record.Request.Key()); !errors.Is(err, aterequest.ErrCorrupt) {
		t.Fatalf("corrupt receipt accepted: %v", err)
	}
	if err := store.Insert(ctx, record); !errors.Is(err, aterequest.ErrCorrupt) {
		t.Fatalf("corrupt receipt allowed identity reuse: %v", err)
	}
}

func TestATERequestsIgnoredWritesCannotClaimDurability(t *testing.T) {
	ctx := t.Context()
	db := openATEPendingDB(t, ":memory:")
	store := db.ATERequests()
	record := atePendingRecord(t, "ignored")
	if _, err := db.db.Exec(`CREATE TRIGGER ignore_insert BEFORE INSERT ON ate_pending_requests BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.Insert(ctx, record); !errors.Is(err, aterequest.ErrConflict) {
		t.Fatalf("missing insert reported durable: %v", err)
	}
	if _, err := db.db.Exec(`DROP TRIGGER ignore_insert`); err != nil {
		t.Fatal(err)
	}
	if err := store.Insert(ctx, record); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`CREATE TRIGGER ignore_attempt BEFORE UPDATE ON ate_pending_requests BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	if started, err := store.BeginAttempt(ctx, record); started || !errors.Is(err, aterequest.ErrConflict) {
		t.Fatalf("missing attempt authorized submission: %v %v", started, err)
	}
	if _, err := db.db.Exec(`DROP TRIGGER ignore_attempt`); err != nil {
		t.Fatal(err)
	}
	if started, err := store.BeginAttempt(ctx, record); err != nil || !started {
		t.Fatalf("attempt: %v %v", started, err)
	}
	result := api.CommitResult{Code: api.CodeRejected, RequestID: record.Request.RequestID}
	if _, err := db.db.Exec(`CREATE TRIGGER ignore_receipt BEFORE INSERT ON ate_rejected_requests BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteRejected(ctx, record, result); !errors.Is(err, aterequest.ErrConflict) {
		t.Fatalf("missing terminal receipt accepted: %v", err)
	}
	if got, found, err := store.Get(ctx, record.Request.Key()); err != nil || !found || !got.Attempted || got.Rejection != nil {
		t.Fatalf("missing receipt destroyed unknown protection: %+v %v", got, err)
	}
	if _, err := db.db.Exec(`DROP TRIGGER ignore_receipt; CREATE TRIGGER ignore_delete BEFORE DELETE ON ate_pending_requests BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteRejected(ctx, record, result); !errors.Is(err, aterequest.ErrConflict) {
		t.Fatalf("missing cleanup reported success: %v", err)
	}
	got, found, err := store.Get(ctx, record.Request.Key())
	if err != nil || !found || got.Rejection == nil || *got.Rejection != result {
		t.Fatalf("cleanup failure lost durable rejection: %+v %v", got, err)
	}
	page, err := store.List(ctx, aterequest.ListOptions{Limit: 1, MaxBytes: 1 << 20})
	if err != nil || len(page) != 1 || page[0].Rejection == nil || *page[0].Rejection != result {
		t.Fatalf("recovery list omitted durable rejection: %+v %v", page, err)
	}
}

func openATEPendingDB(t *testing.T, path string) *Database {
	t.Helper()
	db, err := OpenRuntime(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func assertATEPendingCount(t *testing.T, db *Database, want int) {
	t.Helper()
	var count int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM ate_pending_requests`).Scan(&count); err != nil || count != want {
		t.Fatalf("pending count=%d want=%d err=%v", count, want, err)
	}
	// Native requests must never be mirrored into local history or the legacy
	// journal. This assertion runs before and after acknowledgement.
	for _, table := range []string{"session_events", "orchestration_events", "temporal_journal"} {
		if err := db.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("native request copied into %s: count=%d err=%v", table, count, err)
		}
	}
}

func TestATERequestsInsertOnceAndVerifiedDeletion(t *testing.T) {
	db := openATEPendingDB(t, filepath.Join(t.TempDir(), "requests.db"))
	store := db.ATERequests()
	record := atePendingRecord(t, "stable")
	ctx := t.Context()
	if err := store.Insert(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := store.Insert(ctx, record); err != nil {
		t.Fatalf("identical insert: %v", err)
	}
	for _, change := range []func(*aterequest.Record){
		func(r *aterequest.Record) { r.Request.Body = []byte("changed") },
		func(r *aterequest.Record) { r.Request.Method = "changed/v1" },
		func(r *aterequest.Record) { r.StoreID = "other-store" },
		func(r *aterequest.Record) {
			r.Request.ExpectedFence = &model.CommitFence{Store: r.StoreID, CommitDigest: model.CommitDigest(strings.Repeat("0", 64))}
		},
	} {
		changed := record
		change(&changed)
		changed.Digest, _ = api.CommandDigest(changed.Request)
		if err := store.Insert(ctx, changed); !errors.Is(err, aterequest.ErrConflict) {
			t.Fatalf("changed identity overwritten: %v", err)
		}
	}
	got, found, err := store.Get(ctx, record.Request.Key())
	if err != nil || !found || !aterequest.Equal(got, record) {
		t.Fatalf("original record changed: found=%v err=%v", found, err)
	}
	for _, alter := range []func(*api.CommitResult){
		func(r *api.CommitResult) { r.Code = api.CodeCommitUnknown },
		func(r *api.CommitResult) { r.Code = api.CodeVolatileAck },
		func(r *api.CommitResult) { r.RequestDigest = model.ObjectID(strings.Repeat("b", 64)) },
		func(r *api.CommitResult) { r.RequestID = "another" },
		func(r *api.CommitResult) { r.Fence.Store = "another" },
	} {
		bad := atePendingAck(record)
		alter(&bad)
		if err := store.DeleteAcknowledged(ctx, record, bad); err == nil {
			t.Fatal("unverified acknowledgement deleted payload")
		}
		assertATEPendingCount(t, db, 1)
	}
	changed := record
	changed.Request.Body = []byte("other")
	changed.Digest, _ = api.CommandDigest(changed.Request)
	if err := store.DeleteAcknowledged(ctx, changed, atePendingAck(changed)); !errors.Is(err, aterequest.ErrConflict) {
		t.Fatalf("different record deleted original: %v", err)
	}
	for range 2 {
		if err := store.DeleteAcknowledged(ctx, record, atePendingAck(record)); err != nil {
			t.Fatal(err)
		}
	}
	assertATEPendingCount(t, db, 0)
}

func TestATERequestsBoundsAndPagination(t *testing.T) {
	db := openATEPendingDB(t, ":memory:")
	store := db.ATERequests()
	for _, id := range []string{"c", "a", "b"} {
		if err := store.Insert(t.Context(), atePendingRecord(t, id)); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := aterequest.EncodeRecord(atePendingRecord(t, "a"))
	if err != nil {
		t.Fatal(err)
	}
	for _, options := range []aterequest.ListOptions{
		{}, {Limit: -1, MaxBytes: 100}, {Limit: aterequest.MaxListRecords + 1, MaxBytes: 100},
		{Limit: 1, MaxBytes: 0}, {Limit: 1, MaxBytes: -1},
		{Limit: 1, MaxBytes: aterequest.MaxListBytes + 1}, {Limit: 1, MaxBytes: len(raw) - 1},
	} {
		if _, err := store.List(t.Context(), options); !errors.Is(err, aterequest.ErrLimit) {
			t.Fatalf("invalid bound accepted: %+v err=%v", options, err)
		}
	}
	options := aterequest.ListOptions{Limit: 3, MaxBytes: 2 * len(raw)}
	page, err := store.List(t.Context(), options)
	if err != nil || len(page) != 2 || page[0].Request.RequestID != "a" || page[1].Request.RequestID != "b" {
		t.Fatalf("byte-bound page: %+v err=%v", page, err)
	}
	key := page[1].Request.Key()
	options.After = &key
	page, err = store.List(t.Context(), options)
	if err != nil || len(page) != 1 || page[0].Request.RequestID != "c" {
		t.Fatalf("cursor page: %+v err=%v", page, err)
	}
	options = aterequest.ListOptions{Limit: 1, MaxBytes: aterequest.MaxListBytes}
	page, err = store.List(t.Context(), options)
	if err != nil || len(page) != 1 {
		t.Fatalf("count-bound page: %d err=%v", len(page), err)
	}
	key = api.RequestKey{}
	options.After = &key
	if _, err := store.List(t.Context(), options); !errors.Is(err, aterequest.ErrInvalid) {
		t.Fatalf("invalid cursor accepted: %v", err)
	}
	if _, _, err := store.Get(t.Context(), key); !errors.Is(err, aterequest.ErrInvalid) {
		t.Fatalf("invalid retrieval key accepted: %v", err)
	}
	record := atePendingRecord(t, "oversize")
	record.Request.Body = make([]byte, api.MaxCommandBody+1)
	if err := store.Insert(t.Context(), record); !errors.Is(err, aterequest.ErrInvalid) {
		t.Fatalf("oversize request accepted: %v", err)
	}
	assertATEPendingCount(t, db, 3)
}

func TestATERequestsCorruptionPreventsReplayAndDeletion(t *testing.T) {
	for _, kind := range []string{"checksum", "body", "row-key", "duplicate-field"} {
		t.Run(kind, func(t *testing.T) {
			db := openATEPendingDB(t, ":memory:")
			store := db.ATERequests()
			record := atePendingRecord(t, "stable")
			if err := store.Insert(t.Context(), record); err != nil {
				t.Fatal(err)
			}
			raw, _ := aterequest.EncodeRecord(record)
			key := record.Request.Key()
			switch kind {
			case "checksum":
				if _, err := db.db.Exec(`UPDATE ate_pending_requests SET checksum = ?`, strings.Repeat("0", 64)); err != nil {
					t.Fatal(err)
				}
			case "row-key":
				key.RequestID = "wrong"
				if _, err := db.db.Exec(`UPDATE ate_pending_requests SET request_id = ?`, key.RequestID); err != nil {
					t.Fatal(err)
				}
			default:
				if kind == "body" {
					raw = bytes.Replace(raw, []byte("AP8KgA=="), []byte("AQIDBA=="), 1)
				} else {
					raw = append([]byte(`{"version":1,`), raw[1:]...)
				}
				// Even a valid outer checksum must not mask a bad command digest
				// or an ambiguous internal JSON record.
				if _, err := db.db.Exec(`UPDATE ate_pending_requests SET record = ?, checksum = ?`, raw, sha256Hex(raw)); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := store.Get(t.Context(), key); !errors.Is(err, aterequest.ErrCorrupt) {
				t.Fatalf("corruption accepted: %v", err)
			}
			if _, err := store.List(t.Context(), aterequest.ListOptions{Limit: 1, MaxBytes: aterequest.MaxListBytes}); !errors.Is(err, aterequest.ErrCorrupt) {
				t.Fatalf("list accepted corruption: %v", err)
			}
			if kind != "row-key" {
				if err := store.DeleteAcknowledged(t.Context(), record, atePendingAck(record)); !errors.Is(err, aterequest.ErrCorrupt) {
					t.Fatalf("corrupt record deleted: %v", err)
				}
			}
			assertATEPendingCount(t, db, 1)
		})
	}
}

func TestATERequestsLostResponsesReopen(t *testing.T) {
	for _, beforeCommit := range []bool{true, false} {
		name := "lost-ack"
		if beforeCommit {
			name = "lost-submission"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "requests.db")
			db := openATEPendingDB(t, path)
			directory, err := storage.OpenDirectory(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer directory.Close()
			native, err := engine.Create(directory, engine.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = native.Close() }()
			snapshot, err := native.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			body, err := api.EncodeStateBatch(api.StateBatch{Operations: []api.StateMutation{{
				Key: []byte("key"), Kind: api.MutationPut, Value: api.Value{Kind: api.ValueBytes, Data: []byte{0, 0xff, '\n'}},
			}}})
			if err != nil {
				t.Fatal(err)
			}
			request := api.CommitRequest{Space: "space", Principal: "principal", RequestID: "stable-request",
				Method: api.MethodMutateState, Body: body, ExpectedFence: &snapshot.Fence}
			client := &disconnectedATE{Client: native, lose: true, beforeCommit: beforeCommit}
			executor, err := aterequest.NewExecutor(client, db.ATERequests(), snapshot.Fence.Store)
			if err != nil {
				t.Fatal(err)
			}
			result, err := executor.Execute(ctx, request)
			if !errors.Is(err, aterequest.ErrUnknown) || result.Code != api.CodeCommitUnknown {
				t.Fatalf("disconnect was not unknown: result=%+v err=%v", result, err)
			}
			assertATEPendingCount(t, db, 1)
			original := bytes.Clone(client.commits[0])
			request.Body[0] ^= 0xff
			// A changed body under the same key is rejected locally even while
			// disconnected. It cannot replace the only recoverable original.
			if _, err := executor.Execute(ctx, request); !errors.Is(err, aterequest.ErrConflict) || len(client.commits) != 1 {
				t.Fatalf("changed payload reached ATE: err=%v commits=%d", err, len(client.commits))
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if err := native.Close(); err != nil {
				t.Fatal(err)
			}
			native, _, err = engine.OpenExisting(directory, engine.Options{})
			if err != nil {
				t.Fatal(err)
			}
			db = openATEPendingDB(t, path)
			client = &disconnectedATE{Client: native}
			executor, err = aterequest.NewExecutor(client, db.ATERequests(), snapshot.Fence.Store)
			if err != nil {
				t.Fatal(err)
			}
			result, err = executor.Resume(ctx, request.Key())
			if beforeCommit {
				if !errors.Is(err, aterequest.ErrUnknown) || len(client.commits) != 0 {
					t.Fatalf("uncertain attempt was replayed: result=%+v err=%v", result, err)
				}
				stored, found, err := db.ATERequests().Get(ctx, request.Key())
				if err != nil || !found || !stored.Attempted {
					t.Fatalf("missing attempt guard: found=%v err=%v", found, err)
				}
				saved, err := api.CanonicalCommand(stored.Request)
				if err != nil || !bytes.Equal(saved, original) {
					t.Fatalf("uncertain original bytes changed: %v", err)
				}
				assertATEPendingCount(t, db, 1)
				return
			}
			if err != nil || result.Code != api.CodeDurableAck || result.RequestID != request.RequestID {
				t.Fatalf("restart recovery: result=%+v err=%v", result, err)
			}
			if len(client.commits) != 0 {
				t.Fatal("durable lookup caused another submission")
			}
			assertATEPendingCount(t, db, 0)
			after, err := native.Snapshot(ctx)
			if err != nil || after.Fence != result.Fence || after.Fence.LSN != 1 {
				t.Fatalf("expected exactly one authoritative write: snapshot=%+v err=%v", after, err)
			}
			value, err := native.GetState(ctx, api.ReadStateRequest{Space: "space", Key: []byte("key")})
			if err != nil || !value.Found || !bytes.Equal(value.Value.Data, []byte{0, 0xff, '\n'}) {
				t.Fatalf("authoritative bytes changed: value=%+v err=%v", value, err)
			}
		})
	}
}

func TestATERequestsCleanupFailureIsKnownCommittedAndRetryable(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "requests.db")
	db := openATEPendingDB(t, path)
	directory, err := storage.OpenDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	native, err := engine.Create(directory, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	snapshot, err := native.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	body, err := api.EncodeStateBatch(api.StateBatch{Operations: []api.StateMutation{{
		Key: []byte("key"), Kind: api.MutationPut, Value: api.Value{Kind: api.ValueBytes, Data: []byte("value")},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	request := api.CommitRequest{Space: "space", Principal: "principal", RequestID: "cleanup",
		Method: api.MethodMutateState, Body: body}
	client := &disconnectedATE{Client: native}
	executor, err := aterequest.NewExecutor(client, db.ATERequests(), snapshot.Fence.Store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`CREATE TRIGGER fail_ate_cleanup BEFORE DELETE ON ate_pending_requests
BEGIN SELECT RAISE(ABORT, 'injected cleanup failure'); END`); err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(ctx, request)
	var cleanup *aterequest.CleanupError
	if !errors.As(err, &cleanup) || !errors.Is(err, aterequest.ErrCleanup) ||
		errors.Is(err, aterequest.ErrUnknown) || errors.Is(err, aterequest.ErrRejected) ||
		result.Code != api.CodeDurableAck || cleanup.Result != result || cleanup.Key != request.Key() {
		t.Fatalf("lost known commit on cleanup failure: result=%+v err=%v", result, err)
	}
	assertATEPendingCount(t, db, 1)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openATEPendingDB(t, path)
	if _, err := db.db.Exec(`DROP TRIGGER fail_ate_cleanup`); err != nil {
		t.Fatal(err)
	}
	executor, err = aterequest.NewExecutor(client, db.ATERequests(), snapshot.Fence.Store)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := executor.Resume(ctx, request.Key())
	if err != nil || recovered != result || len(client.commits) != 1 {
		t.Fatalf("cleanup retry resubmitted or changed ack: result=%+v err=%v commits=%d", recovered, err, len(client.commits))
	}
	assertATEPendingCount(t, db, 0)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openATEPendingDB(t, path)
	assertATEPendingCount(t, db, 0)
}

func TestATERequestsTerminalRejectionCleanup(t *testing.T) {
	for _, outcome := range []string{"rejected", "precondition", "idempotency"} {
		for _, failure := range []string{"none", "marker", "delete"} {
			t.Run(outcome+"/"+failure, func(t *testing.T) {
				ctx := t.Context()
				path := filepath.Join(t.TempDir(), "requests.db")
				db := openATEPendingDB(t, path)
				record := atePendingRecord(t, "terminal")
				result := api.CommitResult{Code: api.CodeRejected, RequestID: record.Request.RequestID}
				client := &terminalATE{result: result, err: &protocol.Error{Code: api.ErrorInvalidRequest}}
				if outcome != "rejected" {
					result.Code, result.Conflict = api.CodeConflict, api.ConflictPrecondition
					if outcome == "idempotency" {
						result.Conflict = api.ConflictIdempotency
					}
					client.result, client.err = result, nil
				}
				switch failure {
				case "marker":
					if _, err := db.db.Exec(`CREATE TRIGGER fail_terminal BEFORE INSERT ON ate_rejected_requests
BEGIN SELECT RAISE(ABORT, 'terminal marker unavailable'); END`); err != nil {
						t.Fatal(err)
					}
				case "delete":
					if _, err := db.db.Exec(`CREATE TRIGGER fail_terminal BEFORE DELETE ON ate_pending_requests
BEGIN SELECT RAISE(ABORT, 'payload cleanup unavailable'); END`); err != nil {
						t.Fatal(err)
					}
				}
				executor, err := aterequest.NewExecutor(client, db.ATERequests(), record.StoreID)
				if err != nil {
					t.Fatal(err)
				}
				got, err := executor.Execute(ctx, record.Request)
				if !errors.Is(err, aterequest.ErrRejected) || errors.Is(err, aterequest.ErrUnknown) ||
					errors.Is(err, aterequest.ErrCleanup) || got != result || client.commits != 1 {
					t.Fatalf("terminal response misclassified: result=%+v err=%v commits=%d", got, err, client.commits)
				}
				if failure != "none" {
					var cleanup *aterequest.RejectionCleanupError
					if !errors.As(err, &cleanup) || !errors.Is(err, aterequest.ErrRejectionCleanup) ||
						cleanup.Key != record.Request.Key() || cleanup.Result != result {
						t.Fatalf("missing typed rejection cleanup error: %v", err)
					}
					assertATEPendingCount(t, db, 1)
				} else {
					assertATEPendingCount(t, db, 0)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				db = openATEPendingDB(t, path)
				if failure != "none" {
					if _, err := db.db.Exec(`DROP TRIGGER fail_terminal`); err != nil {
						t.Fatal(err)
					}
				}
				// The same body would now succeed if recovery submitted it.
				later := &terminalATE{accept: true}
				executor, err = aterequest.NewExecutor(later, db.ATERequests(), record.StoreID)
				if err != nil {
					t.Fatal(err)
				}
				_, err = executor.Resume(ctx, record.Request.Key())
				switch failure {
				case "marker":
					if !errors.Is(err, aterequest.ErrUnknown) {
						t.Fatalf("failed terminal write lost its attempt guard: %v", err)
					}
					// The caller retained the exact conclusive response. Only
					// cleanup is retried; no execution is authorized.
					if err := db.ATERequests().DeleteRejected(ctx, record, result); err != nil {
						t.Fatal(err)
					}
				case "delete":
					if !errors.Is(err, aterequest.ErrRejected) {
						t.Fatalf("terminal cleanup was not resumed: %v", err)
					}
				default:
					if !errors.Is(err, aterequest.ErrNotFound) {
						t.Fatalf("deleted payload unexpectedly found: %v", err)
					}
				}
				if later.commits != 0 {
					t.Fatal("previously rejected command was applied after conditions changed")
				}
				assertATEPendingCount(t, db, 0)
				if _, err := executor.Execute(ctx, record.Request); !errors.Is(err, aterequest.ErrRejected) || later.commits != 0 {
					t.Fatalf("terminal identity was reinserted: %v", err)
				}
				changed := record.Request
				changed.Body = []byte("different")
				if _, err := executor.Execute(ctx, changed); !errors.Is(err, aterequest.ErrConflict) {
					t.Fatalf("terminal identity accepted changed bytes: %v", err)
				}
				if err := db.ATERequests().DeleteRejected(ctx, record, result); err != nil {
					t.Fatalf("terminal deletion is not idempotent: %v", err)
				}
				var raw []byte
				if err := db.db.QueryRow(`SELECT record FROM ate_rejected_requests`).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				fence, err := aterequest.DecodeRejection(raw)
				if err != nil || fence.Key != record.Request.Key() || fence.Digest != record.Digest ||
					bytes.Contains(raw, []byte(`"Body"`)) || bytes.Contains(raw, []byte(`"request"`)) {
					t.Fatalf("terminal fence contains invalid identity or request bytes: %s err=%v", raw, err)
				}
			})
		}
	}
}

func TestATERequestsRejectDeletionRequiresExactRecordAndResponse(t *testing.T) {
	db := openATEPendingDB(t, ":memory:")
	store := db.ATERequests()
	record := atePendingRecord(t, "terminal")
	result := api.CommitResult{Code: api.CodeConflict, RequestID: record.Request.RequestID, Conflict: api.ConflictPrecondition}
	if err := store.Insert(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteRejected(t.Context(), record, result); !errors.Is(err, aterequest.ErrConflict) {
		t.Fatalf("unattempted request deleted as rejected: %v", err)
	}
	started, err := store.BeginAttempt(t.Context(), record)
	if err != nil || !started {
		t.Fatalf("begin attempt: started=%v err=%v", started, err)
	}
	if started, err := store.BeginAttempt(t.Context(), record); err != nil || started {
		t.Fatalf("attempt granted twice: started=%v err=%v", started, err)
	}
	changed := record
	changed.Request.Body = []byte("changed")
	changed.Digest, _ = api.CommandDigest(changed.Request)
	if err := store.DeleteRejected(t.Context(), changed, result); !errors.Is(err, aterequest.ErrConflict) {
		t.Fatalf("deletion ignored exact immutable envelope: %v", err)
	}
	for _, alter := range []func(*api.CommitResult){
		func(r *api.CommitResult) { r.RequestID = "other" },
		func(r *api.CommitResult) { r.RequestDigest = record.Digest },
		func(r *api.CommitResult) { r.Fence = atePendingAck(record).Fence },
		func(r *api.CommitResult) { r.Conflict = "" },
		func(r *api.CommitResult) { r.Code = api.CodeCommitUnknown },
		func(r *api.CommitResult) { r.Code = api.CodeNotSubmitted },
	} {
		bad := result
		alter(&bad)
		if err := store.DeleteRejected(t.Context(), record, bad); err == nil {
			t.Fatal("malformed rejection deleted pending body")
		}
		assertATEPendingCount(t, db, 1)
	}
	if err := store.DeleteRejected(t.Context(), record, result); err != nil {
		t.Fatal(err)
	}
	assertATEPendingCount(t, db, 0)
}

func TestATERequestsUnattemptedReopenPreservesExactSubmission(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.db")
	db := openATEPendingDB(t, path)
	record := atePendingRecord(t, "unattempted")
	if err := db.ATERequests().Insert(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openATEPendingDB(t, path)
	stored, found, err := db.ATERequests().Get(t.Context(), record.Request.Key())
	if err != nil || !found || stored.Attempted || !aterequest.Equal(stored, record) {
		t.Fatalf("unattempted envelope changed on reopen: found=%v err=%v", found, err)
	}
	client := &terminalATE{accept: true}
	executor, err := aterequest.NewExecutor(client, db.ATERequests(), record.StoreID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := executor.Resume(t.Context(), record.Request.Key())
	if err != nil || got.RequestDigest != record.Digest || client.commits != 1 {
		t.Fatalf("unattempted envelope did not submit exactly: result=%+v err=%v", got, err)
	}
	assertATEPendingCount(t, db, 0)
}

func TestATERequestsNativeTerminalResponses(t *testing.T) {
	db := openATEPendingDB(t, ":memory:")
	directory, err := storage.OpenDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	native, err := engine.Create(directory, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	ctx := t.Context()
	genesis, err := native.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	body, err := api.EncodeStateBatch(api.StateBatch{Operations: []api.StateMutation{{
		Key: []byte("key"), Kind: api.MutationPut, Value: api.Value{Kind: api.ValueBytes, Data: []byte("value")},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	request := api.CommitRequest{Space: "space", Principal: "principal", RequestID: "seed", Method: api.MethodMutateState, Body: body}
	seed, err := native.Commit(ctx, request)
	if err != nil || seed.Code != api.CodeDurableAck {
		t.Fatalf("seed failed: %+v %v", seed, err)
	}
	executor, err := aterequest.NewExecutor(&classifiedATE{Client: native}, db.ATERequests(), genesis.Fence.Store)
	if err != nil {
		t.Fatal(err)
	}
	for _, rejected := range []bool{false, true} {
		want := api.CodeConflict
		request.RequestID = "stale"
		request.ExpectedFence = &genesis.Fence
		if rejected {
			request.RequestID, request.Method, request.ExpectedFence = "unsupported", "unsupported/v1", nil
			want = api.CodeRejected
		}
		result, err := executor.Execute(ctx, request)
		if !errors.Is(err, aterequest.ErrRejected) || errors.Is(err, aterequest.ErrUnknown) ||
			result.Code != want || result.RequestDigest != "" || result.Fence != (model.CommitFence{}) {
			t.Fatalf("native rejection contract mismatch: result=%+v err=%v", result, err)
		}
		assertATEPendingCount(t, db, 0)
		if _, err := executor.Resume(ctx, request.Key()); !errors.Is(err, aterequest.ErrNotFound) {
			t.Fatalf("native rejection left replay bytes: %v", err)
		}
	}
	after, err := native.Snapshot(ctx)
	if err != nil || after.Fence != seed.Fence {
		t.Fatalf("rejected commands changed native state: %+v %v", after, err)
	}
}

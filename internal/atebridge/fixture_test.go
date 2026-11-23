package atebridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	ate "github.com/vkviyu/agent-temporal-engine"
	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/memory"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/agent-temporal-engine/service"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateports"
	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimestore"
)

// Fail instead of hanging if a native StateStore reacquires a leased process.
type nonNestedProcess struct {
	Process
	leased atomic.Bool
}

func (p *nonNestedProcess) WithClient(ctx context.Context, fn func(*client.Client) error) error {
	if p.leased.Swap(true) {
		return errors.New("nested process lease")
	}
	defer p.leased.Store(false)
	return p.Process.WithClient(ctx, fn)
}

type nativeTestState struct{ authority *ateauthority.Store }

func (s nativeTestState) Get(ctx context.Context, key string) (json.RawMessage, bool, error) {
	var raw json.RawMessage
	var found bool
	err := s.authority.View(ctx, func(tx *ateauthority.Tx) error {
		var err error
		found, err = tx.Get(key, &raw)
		return err
	})
	return raw, found, err
}

func (s nativeTestState) Put(ctx context.Context, key string, value json.RawMessage) error {
	return s.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		var prior json.RawMessage
		found, err := tx.Get(key, &prior)
		if err != nil {
			return err
		}
		if found {
			if !bytes.Equal(prior, value) {
				return ateports.ErrConflict
			}
			return nil
		}
		return tx.Create(key, value)
	})
}

func (f *fixture) nativeOptions(t *testing.T) Options {
	t.Helper()
	process := &nonNestedProcess{Process: f.p}
	state, err := ateauthority.New(ateauthority.Options{
		Process: process, Pending: f.db.ATERequests(), Space: "gsea-host-state", Principal: "gsea",
	})
	if err != nil {
		t.Fatal(err)
	}
	return Options{Process: process, Pending: f.db.ATERequests(), State: nativeTestState{state},
		Namespace: "gsea:test-host", OperationalSpace: "gsea-native-executions"}
}

func (f *fixture) useNative(t *testing.T) {
	t.Helper()
	var err error
	f.b, err = New(f.nativeOptions(t))
	if err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) pendingRequests(t *testing.T) []aterequest.Record {
	t.Helper()
	records, err := f.db.ATERequests().List(t.Context(), aterequest.ListOptions{
		Limit: aterequest.MaxListRecords, MaxBytes: aterequest.MaxListBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	return records
}

// Every lease uses a fresh authenticated pipe and the real durable engine.
type pipeProcess struct {
	mu      sync.Mutex
	store   *ate.Store
	path    string
	calls   int
	commits []api.CommitRequest
	drop    string // before or after the next commit
	check   func(api.CommitRequest) error
}

type observedBackend struct {
	*ate.Store
	process *pipeProcess
	conn    net.Conn
}

func (o *observedBackend) Commit(ctx context.Context, request api.CommitRequest) (api.CommitResult, error) {
	p := o.process
	p.commits = append(p.commits, request)
	if p.check != nil {
		if err := p.check(request); err != nil {
			return api.CommitResult{}, err
		}
	}
	drop := p.drop
	p.drop = ""
	if drop == "before" {
		_ = o.conn.Close()
		return api.CommitResult{}, errors.New("injected disconnect before commit")
	}
	result, err := o.Store.Commit(ctx, request)
	if drop == "after" {
		_ = o.conn.Close()
	}
	return result, err
}

func (p *pipeProcess) WithClient(ctx context.Context, fn func(*client.Client) error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	a, z := net.Pipe()
	server, err := service.New(&observedBackend{Store: p.store, process: p, conn: z}, service.Options{
		Credentials: map[string]service.Credential{"test-token": {Principal: "gsea", AllSpaces: true, AllowGlobalSnapshot: true}},
	})
	if err != nil {
		_ = a.Close()
		_ = z.Close()
		return err
	}
	done := make(chan error, 1)
	go func() { done <- server.ServeStream(ctx, z) }()
	defer func() { _ = a.Close(); _ = server.Shutdown(context.Background()); <-done }()
	c, err := client.New(ctx, a, client.Options{Token: "test-token"})
	if err != nil {
		return err
	}
	defer c.Close()
	return fn(c)
}

func (p *pipeProcess) restart(t *testing.T) {
	t.Helper()
	if err := p.store.Close(); err != nil {
		t.Fatal(err)
	}
	s, report, err := ate.OpenExisting(p.path, ate.Options{AgentProfile: true})
	if err != nil || !report.Verified {
		t.Fatalf("native replay: %+v %v", report, err)
	}
	p.store = s
}

type fixture struct {
	b      *Bridge
	p      *pipeProcess
	db     *runtimestore.Database
	dbPath string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	store, err := ate.Create(dir, ate.Options{AgentProfile: true})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{p: &pipeProcess{store: store, path: dir}, dbPath: filepath.Join(t.TempDir(), "session.db")}
	f.db, err = runtimestore.OpenRuntime(ctx, f.dbPath)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	f.b, err = New(f.nativeOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.db.Close(); _ = f.p.store.Close() })
	return f
}

func (f *fixture) restart(t *testing.T) {
	t.Helper()
	f.p.restart(t)
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.db, err = runtimestore.OpenRuntime(context.Background(), f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	f.b, err = New(f.nativeOptions(t))
	if err != nil {
		t.Fatal(err)
	}
}

func readSource(t *testing.T, b *Bridge, source memory.Source) []byte {
	t.Helper()
	var data []byte
	err := b.WithClient(context.Background(), func(c *client.Client) error {
		view, err := c.OpenView(context.Background(), api.OpenViewRequest{Space: source.Anchor.Space, Anchor: &source.Anchor})
		if err != nil {
			return err
		}
		defer c.CloseView(context.Background(), api.CloseViewRequest{Ref: view.Ref})
		for {
			part, err := c.ReadContent(context.Background(), api.ReadContentRequest{
				View: view.Ref, Ref: model.NodeRef{Kind: model.NodeObject, Object: source.SourceID},
				Projection: "value_bytes", Offset: uint64(len(data)), Length: api.MaxContentChunkBytes,
			})
			if err != nil {
				return err
			}
			data = append(data, part.Data...)
			if part.EOF {
				return nil
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

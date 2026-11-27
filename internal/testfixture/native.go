// Package testfixture wires real native ATE authorities for integration tests.
// It must only be imported by tests. SQLite owns runtime resources and pending
// request envelopes; business records and session history live in ATE.
package testfixture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	ate "github.com/vkviyu/agent-temporal-engine"
	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/agent-temporal-engine/service"
	"github.com/vkviyu/agent-temporal-engine/spacecatalog"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentprofile"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	orchestrationate "github.com/vkviyu/go-self-evolving-agent/internal/orchestration/atestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimestore"
	sessionate "github.com/vkviyu/go-self-evolving-agent/internal/session/atestore"
)

// AuthoritySpace matches production assembly. Tests and CLI subprocesses must
// address the same native namespace or they silently read different history.
const AuthoritySpace model.SpaceID = "gsea-authority"

// OrchestrationStore fills a durable profile snapshot for fixtures that do not
// exercise profile selection. Production assembly always passes the exact
// encoded profile; explicit snapshots are preserved unchanged.
type OrchestrationStore struct{ *orchestrationate.Store }

func fixtureSnapshot(revision string) json.RawMessage {
	if revision == "" {
		revision = "fixture-v1"
	}
	name := strings.TrimSuffix(revision, "-v1")
	if name == "" || name == revision {
		name = "fixture"
	}
	raw, err := agentprofile.Encode(agentprofile.Profile{
		Name: name, Revision: revision, ModelProfile: "test", Instruction: "fixture",
		WorkspaceMode: agentprofile.WorkspaceSharedLease, ContextInheritance: agentprofile.InheritNone,
		Limits:   agentprofile.Limits{MaxIterations: 4, MaxToolCalls: 8, MaxChildren: 16, MaxWallTime: time.Minute},
		CanSpawn: true, CanManageTasks: true, CanSendMessages: true,
	})
	if err != nil {
		panic(err)
	}
	return raw
}

func (s *OrchestrationStore) EnsureRootThread(ctx context.Context, seed orchestration.RootThreadSeed) (orchestration.RootThread, bool, error) {
	if len(seed.ProfileSnapshot) == 0 {
		if seed.ProfileRevision == "" {
			seed.ProfileRevision = "fixture-v1"
		}
		seed.ProfileSnapshot = fixtureSnapshot(seed.ProfileRevision)
	}
	return s.Store.EnsureRootThread(ctx, seed)
}

// Database groups independent native business stores and local runtime stores.
// Close releases both, so opening the same path verifies actual ATE recovery.
type Database struct {
	*runtimestore.Database
	Process   *Process
	Authority *ateauthority.Store
	records   *OrchestrationStore
	events    *sessionate.Store
	closeOnce sync.Once
	closeErr  error
}

// Open uses path for runtime SQLite and path+".ate" for native authority files.
// ":memory:" requests an isolated temporary fixture, including native storage.
func Open(t testing.TB, ctx context.Context, path string) (*Database, error) {
	t.Helper()
	if path == ":memory:" {
		path = filepath.Join(t.TempDir(), "runtime.db")
	}
	return OpenWithNativeDir(t, ctx, path, path+".ate")
}

// OpenWithNativeDir binds the fixture to the same runtime database and native
// authority directory that production assembly resolves, so CLI subprocesses
// opened on that directory observe the fixture's records.
func OpenWithNativeDir(t testing.TB, ctx context.Context, path, nativePath string) (_ *Database, err error) {
	t.Helper()
	runtime, err := runtimestore.OpenRuntime(ctx, path)
	if err != nil {
		return nil, err
	}
	f := &Database{Database: runtime}
	defer func() {
		if err != nil {
			_ = f.Close()
		}
	}()
	options := ate.Options{AgentProfile: true, EventProfile: true}
	var engine *ate.Store
	if _, statErr := os.Stat(nativePath); errors.Is(statErr, os.ErrNotExist) {
		if err = os.Mkdir(nativePath, 0700); err != nil {
			return nil, err
		}
		engine, err = ate.Create(nativePath, options)
	} else if statErr != nil {
		return nil, statErr
	} else {
		var verified bool
		engine, report, openErr := ate.OpenExisting(nativePath, options)
		err = openErr
		verified = report.Verified && report.PublicationSynced
		if err == nil && !verified {
			_ = engine.Close()
			return nil, errors.New("native fixture recovery did not verify publication")
		}
		if err == nil {
			f.Process = &Process{engine: engine}
		}
	}
	if err != nil {
		return nil, err
	}
	if f.Process == nil {
		f.Process = &Process{engine: engine}
		result, applyErr := engine.ApplySpace(ctx, api.ApplySpaceRequest{
			Key:     model.RequestKey{Space: AuthoritySpace, Principal: "gsea", RequestID: "create-authority"},
			Command: spacecatalog.Apply{Name: "Test authority"},
		})
		if applyErr != nil {
			return nil, applyErr
		}
		if result.Code != api.CodeDurableAck {
			return nil, fmt.Errorf("create native session space: %+v", result)
		}
	}
	f.Authority, err = ateauthority.New(ateauthority.Options{
		Process: f.Process, Pending: runtime.ATERequests(), Space: AuthoritySpace, Principal: "gsea",
	})
	if err != nil {
		return nil, err
	}
	snapshot, err := f.Process.engine.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	f.events, err = sessionate.New(sessionate.Options{
		Process: f.Process, Pending: runtime.ATERequests(), Space: AuthoritySpace, StoreID: snapshot.Fence.Store,
	})
	if err != nil {
		return nil, err
	}
	store, err := orchestrationate.New(f.Authority, f.events)
	if err != nil {
		return nil, err
	}
	f.records = &OrchestrationStore{Store: store}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Errorf("close native fixture: %v", err)
		}
	})
	return f, nil
}

func (f *Database) Orchestration() *OrchestrationStore { return f.records }
func (f *Database) SessionEvents() *sessionate.Store   { return f.events }

// Close releases the native engine and fixture database once and reports cleanup errors.
func (f *Database) Close() error {
	f.closeOnce.Do(func() {
		if f.Process != nil {
			f.Process.mu.Lock()
			defer f.Process.mu.Unlock()
			f.closeErr = f.Process.engine.Close()
		}
		f.closeErr = errors.Join(f.closeErr, f.Database.Close())
	})
	return f.closeErr
}

// Process uses the real authenticated service/client protocol over an in-process
// stream, serializing leases just like the production process manager.
type Process struct {
	engine *ate.Store
	mu     sync.Mutex
}

func (p *Process) WithClient(ctx context.Context, fn func(*client.Client) error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	server, err := service.New(p.engine, service.Options{Credentials: map[string]service.Credential{
		"test": {Principal: "gsea", AllSpaces: true, AllowGlobalSnapshot: true},
	}})
	if err != nil {
		return err
	}
	defer server.Shutdown(context.Background())
	a, b := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- server.ServeStream(ctx, b) }()
	defer func() { _ = a.Close(); <-done }()
	c, err := client.New(ctx, a, client.Options{Token: "test"})
	if err != nil {
		return err
	}
	defer c.Close()
	return fn(c)
}

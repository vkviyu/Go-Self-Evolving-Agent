package agentflow_test

import (
	"context"
	"net"
	"testing"

	ate "github.com/vkviyu/agent-temporal-engine"
	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/agent-temporal-engine/service"
	"github.com/vkviyu/agent-temporal-engine/spacecatalog"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	orchestrationate "github.com/vkviyu/go-self-evolving-agent/internal/orchestration/atestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimestore"
	sessionate "github.com/vkviyu/go-self-evolving-agent/internal/session/atestore"
)

type flowNativeProcess struct{ engine *ate.Store }

func (p *flowNativeProcess) WithClient(ctx context.Context, fn func(*client.Client) error) error {
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

// flowAuthorityOpener reopens the actual native store between runtime instances.
// SQLite contains only runtime snapshots and pending request envelopes.
func flowAuthorityOpener(t *testing.T) func(*runtimestore.Database) (*orchestrationate.Store, *sessionate.Store) {
	t.Helper()
	ctx := t.Context()
	path := t.TempDir()
	engine, err := ate.Create(path, ate.Options{AgentProfile: true, EventProfile: true})
	if err != nil {
		t.Fatal(err)
	}
	process := &flowNativeProcess{engine: engine}
	t.Cleanup(func() { _ = process.engine.Close() })
	result, err := engine.ApplySpace(ctx, api.ApplySpaceRequest{
		Key:     model.RequestKey{Space: "flow", Principal: "gsea", RequestID: "create-flow"},
		Command: spacecatalog.Apply{Name: "Flow test"},
	})
	if err != nil || result.Code != api.CodeDurableAck {
		t.Fatalf("create authority: %+v, %v", result, err)
	}
	return func(db *runtimestore.Database) (*orchestrationate.Store, *sessionate.Store) {
		t.Helper()
		if err := process.engine.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, report, err := ate.OpenExisting(path, ate.Options{AgentProfile: true, EventProfile: true})
		if err != nil {
			t.Fatal(err)
		}
		process.engine = reopened
		if !report.Verified || !report.PublicationSynced {
			t.Fatal("authority reopen did not verify its publication")
		}
		snapshot, err := reopened.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		records, err := ateauthority.New(ateauthority.Options{Process: process, Pending: db.ATERequests(), Space: "flow", Principal: "gsea"})
		if err != nil {
			t.Fatal(err)
		}
		events, err := sessionate.New(sessionate.Options{Process: process, Pending: db.ATERequests(), Space: "flow", StoreID: snapshot.Fence.Store})
		if err != nil {
			t.Fatal(err)
		}
		store, err := orchestrationate.New(records, events)
		if err != nil {
			t.Fatal(err)
		}
		return store, events
	}
}

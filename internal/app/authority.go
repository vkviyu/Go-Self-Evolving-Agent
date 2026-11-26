package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/agent-temporal-engine/spacecatalog"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateprocess"
	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	sessionate "github.com/vkviyu/go-self-evolving-agent/internal/session/atestore"
)

// authoritySpace is an infrastructure namespace, independent of the semantic
// Spaces that the Agent discovers and organizes for reusable knowledge.
const authoritySpace model.SpaceID = "gsea-authority"

type (
	authorityResources struct {
		process  *ateprocess.Manager
		lease    *ateprocess.ClientLease
		records  *ateauthority.Store
		sessions *sessionate.Store
		storeID  model.StoreID
		bound    authorityProcess
	}
	authorityPendingStore interface {
		aterequest.PendingStore
		BindAuthority(context.Context, model.StoreID) error
	}
	authorityProcess struct {
		manager *ateprocess.Manager
		lease   *ateprocess.ClientLease
		storeID model.StoreID
	}
)

func (p authorityProcess) currentLease() *ateprocess.ClientLease {
	if p.manager != nil {
		if lease := p.manager.CurrentLease(); lease != nil {
			return lease
		}
	}
	return p.lease
}

// WithClient checks store identity on every lease, including after another
// process has replaced a directory while this App was idle.
func (p authorityProcess) WithClient(ctx context.Context, fn func(*client.Client) error) error {
	if ctx == nil || fn == nil || p.manager == nil {
		return aterequest.ErrInvalid
	}
	if scope, ok := ctx.Value(storageBatchKey{}).(*storageBatchScope); ok {
		return scope.withClient(ctx, p, fn)
	}
	if lease := p.currentLease(); lease != nil {
		return lease.WithClientContext(ctx, func(operation context.Context, c *client.Client) error {
			if err := p.validateClient(operation, c); err != nil {
				return err
			}
			err := fn(c)
			if errors.Is(err, aterequest.ErrUnknown) {
				_ = c.Close()
			}
			return err
		})
	}
	var mu sync.Mutex
	var started, returned bool
	var callbackErr error
	done := make(chan struct{})
	err := p.manager.WithClient(ctx, func(c *client.Client) (result error) {
		mu.Lock()
		if returned {
			mu.Unlock()
			return errors.New("authority lease ended before its callback started")
		}
		started = true
		mu.Unlock()
		defer func() {
			callbackErr = result
			close(done)
		}()
		if err := p.validateClient(ctx, c); err != nil {
			return err
		}
		return fn(c)
	})
	mu.Lock()
	returned = true
	invoked := started
	mu.Unlock()
	if invoked {
		// Transaction callbacks are synchronous host code. The manager closes
		// the client on timeout; let bounded reads unwind before callers inspect
		// their results or close the pending-request database.
		<-done
		if callbackErr == nil {
			// A completed callback has a verified result. A later shutdown
			// failure must not encourage a fresh logical mutation.
			return nil
		}
		return errors.Join(callbackErr, err)
	}
	return err
}

func (p authorityProcess) WithFreshClient(ctx context.Context, fn func(*client.Client) error) error {
	if ctx == nil || fn == nil || p.manager == nil {
		return aterequest.ErrInvalid
	}
	if _, ok := ctx.Value(storageBatchKey{}).(*storageBatchScope); ok {
		// Reconciliation must wait until the enclosing batch callback unwinds.
		// Rotating the connection here would wait for this callback's gate.
		return aterequest.ErrLeaseClosed
	}
	return p.manager.WithFreshClient(ctx, func(c *client.Client) error {
		if err := p.validateClient(ctx, c); err != nil {
			return err
		}
		return fn(c)
	})
}

func (p authorityProcess) validateClient(ctx context.Context, c *client.Client) error {
	if err := verifyAuthorityClient(c); err != nil {
		return err
	}
	snapshot, err := c.Snapshot(ctx)
	if err != nil {
		return err
	}
	if p.storeID != "" && snapshot.Fence.Store != p.storeID {
		return fmt.Errorf("%w: ATE authority changed during this runtime", aterequest.ErrConflict)
	}
	return nil
}

func verifyAuthorityClient(c *client.Client) error {
	identity := c.Identity()
	if identity.Principal != "gsea" {
		return errors.New("unexpected ATE authenticated principal")
	}
	for _, capability := range []string{api.AgentSessionReadCapability, "core/state-page-v1"} {
		if !slices.Contains(identity.Capabilities, capability) {
			return fmt.Errorf("ATE binary lacks %s; rebuild the paired ate and gsea binaries", capability)
		}
	}
	return nil
}

// openAuthority establishes the native authority before executable services
// receive repositories. Only unresolved exact requests are recovered locally.
func openAuthority(ctx context.Context, cfg config.Config, pending authorityPendingStore) (*authorityResources, error) {
	if !cfg.ATE.Enabled {
		return nil, errors.New("ATE is required for session and task persistence; enable ate.enabled")
	}
	process, err := ateprocess.New(ateprocess.Options{
		Binary: cfg.ATE.Binary, Directory: cfg.ATE.Directory, Timeout: cfg.ATE.Timeout,
		StartupTimeout: cfg.ATE.StartupTimeout,
	})
	if err != nil {
		return nil, err
	}
	ready := false
	defer func() {
		if !ready {
			_ = process.Close()
		}
	}()
	lease, err := process.OpenClient(ctx)
	if err != nil {
		return nil, err
	}
	readyLease := false
	defer func() {
		if !readyLease {
			_ = lease.Close()
		}
	}()
	bound := authorityProcess{manager: process, lease: lease}
	var storeID model.StoreID
	err = lease.WithClientContext(ctx, func(operation context.Context, c *client.Client) error {
		if err := verifyAuthorityClient(c); err != nil {
			return err
		}
		snapshot, err := c.Snapshot(operation)
		if err != nil {
			return err
		}
		storeID = snapshot.Fence.Store
		if err := pending.BindAuthority(operation, storeID); err != nil {
			return err
		}
		executor, err := aterequest.NewExecutor(c, pending, storeID)
		if err != nil {
			return err
		}
		if err := recoverAuthorityRequests(operation, executor, pending, storeID); err != nil {
			return err
		}
		if err := ensureAuthoritySpace(operation, c, executor); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("open ATE authority: %w", err)
	}
	bound.storeID = storeID
	records, err := ateauthority.New(ateauthority.Options{
		Process: bound, Pending: pending, Space: authoritySpace, Principal: "gsea",
	})
	if err != nil {
		return nil, err
	}
	sessions, err := sessionate.New(sessionate.Options{
		Process: bound, Pending: pending, Space: authoritySpace, StoreID: storeID,
		ReconciliationTimeout: cfg.ATE.Timeout + time.Second,
	})
	if err != nil {
		return nil, err
	}
	resources := &authorityResources{process: process, lease: lease, records: records, sessions: sessions, storeID: storeID, bound: bound}
	// Some callers (offline maintenance) acquire a specific Session lease at
	// BindAuthority. Only that explicit scope may advance saved Session plans.
	if scoped, ok := pending.(interface {
		recoverOwnedStartup(context.Context, *authorityResources) error
	}); ok {
		if err := scoped.recoverOwnedStartup(ctx, resources); err != nil {
			return nil, err
		}
	}
	readyLease = true
	ready = true
	return resources, nil
}

// recoverAuthorityRequests reconciles durable ACKs and finishes locally saved
// rejections. Startup cannot submit preexisting business commands. Session plans
// stay together until their owner can recover dependencies in publication order.
func recoverAuthorityRequests(ctx context.Context, executor *aterequest.Executor, pending aterequest.PendingStore, storeID model.StoreID) error {
	var after *api.RequestKey
	for {
		records, err := pending.List(ctx, aterequest.ListOptions{
			After: after, Limit: aterequest.MaxListRecords, MaxBytes: aterequest.MaxListBytes,
		})
		if err != nil {
			return err
		}
		if len(records) == 0 {
			return nil
		}
		for _, record := range records {
			if record.StoreID != storeID {
				return errors.New("pending request belongs to a different ATE store")
			}
			if err := record.Validate(); err != nil {
				return err
			}
			if sessionate.OwnsPendingRequest(record.Request) {
				continue
			}
			if _, err := executor.ReconcileAcknowledged(ctx, record.Request.Key()); err != nil {
				if errors.Is(err, aterequest.ErrRejected) && !errors.Is(err, aterequest.ErrRejectionCleanup) {
					continue
				}
				return fmt.Errorf("reconcile ATE request %s: %w", record.Request.RequestID, err)
			}
		}
		key := records[len(records)-1].Request.Key()
		after = &key
	}
}

// ensureAuthoritySpace uses a stable creation identity without overwriting
// metadata that has subsequently been revised by the host.
func ensureAuthoritySpace(ctx context.Context, c *client.Client, executor *aterequest.Executor) error {
	existing, err := c.GetSpace(ctx, api.GetSpaceRequest{Space: authoritySpace, Principal: "gsea"})
	if err != nil {
		return err
	}
	if existing.Found {
		return nil
	}
	body, err := spacecatalog.EncodeApply(spacecatalog.Apply{
		Name:        "GSEA execution authority",
		Description: "Authoritative sessions, tasks, messages and execution facts owned by the trusted GSEA host.",
	})
	if err != nil {
		return err
	}
	_, err = executor.Execute(ctx, api.CommitRequest{
		Space: authoritySpace, Principal: "gsea", RequestID: "gsea-authority-create-v1",
		Method: api.MethodApplySpace, Body: body,
	})
	return err
}

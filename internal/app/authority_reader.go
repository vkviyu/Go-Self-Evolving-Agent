package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateprocess"
	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	sessionate "github.com/vkviyu/go-self-evolving-agent/internal/session/atestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/sessionregistry"
)

// SessionCatalog owns one read-only ATE lease used while browsing and
// selecting a session. OpenWithOptions can adopt it so resume keeps the same
// native process for the rest of the invocation.
type SessionCatalog struct {
	resources *authorityResources
}

// OpenSessionCatalog opens the existing ATE store once for session discovery.
// The returned catalog is safe to close when discovery is canceled or when no
// conversation is selected.
func OpenSessionCatalog(ctx context.Context, cfg config.Config) (*SessionCatalog, error) {
	resources, err := openChatCatalogReader(ctx, cfg)
	if err != nil || resources == nil {
		return nil, err
	}
	return &SessionCatalog{resources: resources}, nil
}

// Close releases the catalog lease unless it has been adopted by App.
func (c *SessionCatalog) Close() error {
	if c == nil || c.resources == nil {
		return nil
	}
	err := c.resources.process.Close()
	c.resources = nil
	return err
}

func (c *SessionCatalog) read(ctx context.Context, id session.ID, requireHistory bool) ([]sessionregistry.Session, []session.ChatHistory, error) {
	if c == nil || c.resources == nil {
		return nil, nil, errors.New("session catalog is closed")
	}
	return readChatCatalog(ctx, c.resources, id, requireHistory)
}

// ReadChatCatalog reads catalog and native history through this catalog's
// already-open ATE lease.
func (c *SessionCatalog) ReadChatCatalog(ctx context.Context, id session.ID, requireHistory bool) ([]sessionregistry.Session, []session.ChatHistory, error) {
	return c.read(ctx, id, requireHistory)
}

// adopt transfers the native lease to the application. The catalog becomes a
// no-op close handle after a successful transfer.
func (c *SessionCatalog) adopt(ctx context.Context, cfg config.Config, pending authorityPendingStore) (*authorityResources, error) {
	if c == nil || c.resources == nil {
		return nil, errors.New("session catalog is closed")
	}
	resources := c.resources
	authority, err := adoptAuthority(ctx, cfg, pending, resources)
	if err != nil {
		return nil, err
	}
	c.resources = nil
	return authority, nil
}

func adoptAuthority(ctx context.Context, cfg config.Config, pending authorityPendingStore, resources *authorityResources) (*authorityResources, error) {
	if resources == nil || resources.process == nil || resources.lease == nil {
		return nil, errors.New("session catalog has no native lease")
	}
	bound := authorityProcess{manager: resources.process, lease: resources.lease, storeID: resources.storeID}
	err := resources.lease.WithClientContext(ctx, func(operation context.Context, c *client.Client) error {
		if err := verifyAuthorityClient(c); err != nil {
			return err
		}
		snapshot, err := c.Snapshot(operation)
		if err != nil {
			return err
		}
		if snapshot.Fence.Store != resources.storeID {
			return fmt.Errorf("%w: ATE authority changed during session selection", aterequest.ErrConflict)
		}
		if err := pending.BindAuthority(operation, resources.storeID); err != nil {
			return err
		}
		executor, err := aterequest.NewExecutor(c, pending, resources.storeID)
		if err != nil {
			return err
		}
		if err := recoverAuthorityRequests(operation, executor, pending, resources.storeID); err != nil {
			return err
		}
		return ensureAuthoritySpace(operation, c, executor)
	})
	if err != nil {
		return nil, fmt.Errorf("adopt ATE authority: %w", err)
	}
	records, err := ateauthority.New(ateauthority.Options{
		Process: bound, Pending: pending, Space: authoritySpace, Principal: "gsea",
	})
	if err != nil {
		return nil, err
	}
	sessions, err := sessionate.New(sessionate.Options{
		Process: bound, Pending: pending, Space: authoritySpace, StoreID: resources.storeID,
		ReconciliationTimeout: cfg.ATE.Timeout + time.Second,
	})
	if err != nil {
		return nil, err
	}
	return &authorityResources{
		process: resources.process, lease: resources.lease, records: records, sessions: sessions,
		storeID: resources.storeID, bound: bound,
	}, nil
}

func readChatCatalog(ctx context.Context, resources *authorityResources, id session.ID, requireHistory bool) (entries []sessionregistry.Session, histories []session.ChatHistory, err error) {
	registry, err := sessionregistry.NewAuthority(resources.records, nil)
	if err != nil {
		return nil, nil, err
	}
	if id == "" {
		entries, err = registry.List(ctx)
		if err != nil {
			return nil, nil, err
		}
	} else {
		item, getErr := registry.Get(ctx, string(id))
		if getErr == nil {
			entries = []sessionregistry.Session{item}
			if !requireHistory {
				return entries, nil, nil
			}
		} else if !errors.Is(getErr, sessionregistry.ErrNotFound) {
			return nil, nil, getErr
		}
	}
	histories, err = discoverChats(ctx, resources, id)
	return entries, histories, err
}

// openChatCatalogReader opens existing ATE facts without binding a runtime or
// reconciling its saved commands. A fresh, empty store has no chat catalog.
func openChatCatalogReader(ctx context.Context, cfg config.Config) (_ *authorityResources, err error) {
	options, err := cfg.ResolveATE()
	if err != nil {
		return nil, err
	}
	if !options.Enabled {
		return nil, errors.New("ATE is required for session history; enable ate.enabled")
	}
	dir, err := os.Open(options.Directory)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect ATE directory: %w", err)
	}
	_, readErr := dir.Readdirnames(1)
	if err := dir.Close(); err != nil {
		return nil, err
	}
	if errors.Is(readErr, io.EOF) {
		return nil, nil
	}
	if readErr != nil {
		return nil, fmt.Errorf("inspect ATE directory: %w", readErr)
	}
	process, err := ateprocess.New(ateprocess.Options{
		Binary: options.Binary, Directory: options.Directory, Timeout: options.Timeout, ExistingOnly: true, NoWaitForOwner: true,
		StartupTimeout: options.StartupTimeout,
	})
	if err != nil {
		return nil, err
	}
	keepOpen := false
	defer func() {
		if !keepOpen {
			err = errors.Join(err, process.Close())
		}
	}()
	lease, err := process.OpenClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("open session history: %w", err)
	}
	resources := &authorityResources{process: process, lease: lease}
	found := false
	err = lease.WithClientContext(ctx, func(operation context.Context, c *client.Client) error {
		if err := verifyAuthorityClient(c); err != nil {
			return err
		}
		snapshot, err := c.Snapshot(operation)
		if err != nil {
			return err
		}
		space, err := c.GetSpace(operation, api.GetSpaceRequest{Space: authoritySpace, Principal: "gsea"})
		if err != nil {
			return err
		}
		found = space.Found
		resources.storeID = snapshot.Fence.Store
		return nil
	})
	if err != nil || !found {
		return nil, err
	}
	bound := authorityProcess{manager: process, lease: lease, storeID: resources.storeID}
	pending := readOnlyAuthorityPending{}
	resources.records, err = ateauthority.New(ateauthority.Options{Process: bound, Pending: pending, Space: authoritySpace, Principal: "gsea"})
	if err != nil {
		return nil, err
	}
	resources.sessions, err = sessionate.New(sessionate.Options{Process: bound, Pending: pending, Space: authoritySpace, StoreID: resources.storeID})
	if err != nil {
		return nil, err
	}
	resources.bound = bound
	keepOpen = true
	return resources, nil
}

// A rejecting pending store makes accidental writes fail before submission.
type readOnlyAuthorityPending struct{}

func (readOnlyAuthorityPending) Insert(context.Context, aterequest.Record) error {
	return ateauthority.ErrReadOnly
}
func (readOnlyAuthorityPending) Get(context.Context, api.RequestKey) (aterequest.Record, bool, error) {
	return aterequest.Record{}, false, ateauthority.ErrReadOnly
}
func (readOnlyAuthorityPending) List(context.Context, aterequest.ListOptions) ([]aterequest.Record, error) {
	return nil, ateauthority.ErrReadOnly
}
func (readOnlyAuthorityPending) BeginAttempt(context.Context, aterequest.Record) (bool, error) {
	return false, ateauthority.ErrReadOnly
}
func (readOnlyAuthorityPending) DeleteAcknowledged(context.Context, aterequest.Record, api.CommitResult) error {
	return ateauthority.ErrReadOnly
}
func (readOnlyAuthorityPending) DeleteRejected(context.Context, aterequest.Record, api.CommitResult) error {
	return ateauthority.ErrReadOnly
}

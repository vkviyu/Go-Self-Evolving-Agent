package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateprocess"
	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	sessionate "github.com/vkviyu/go-self-evolving-agent/internal/session/atestore"
)

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

package app

import (
	"context"
	"errors"

	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	orchestrationate "github.com/vkviyu/go-self-evolving-agent/internal/orchestration/atestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

// offlineSessionPending acquires the shared Session lock at the identity
// binding boundary, BEFORE openAuthority reconciles native commands. The App
// value is only a lock holder; no executable runtime services are constructed.
type offlineSessionPending struct {
	authorityPendingStore
	owner       *App
	id          session.ID
	preexisting map[api.RequestKey]bool
}

func (p *offlineSessionPending) BindAuthority(ctx context.Context, id model.StoreID) error {
	p.owner.authority = &authorityResources{storeID: id}
	p.owner.stateMu.Lock()
	err := p.owner.ownSession(p.id)
	p.owner.stateMu.Unlock()
	if err != nil {
		return err
	}
	return p.authorityPendingStore.BindAuthority(ctx, id)
}

func (p *offlineSessionPending) BeginAttempt(ctx context.Context, record aterequest.Record) (bool, error) {
	if p.preexisting[record.Request.Key()] {
		// Attempted envelopes can only be looked up, never resubmitted. For an
		// unattempted envelope there is no generic native Session scope proof:
		// do not execute possibly foreign work under this Session's lock.
		return false, errors.New("unattempted pending command requires execution-owner scope reconciliation before offline maintenance")
	}
	return p.authorityPendingStore.BeginAttempt(ctx, record)
}

func withOfflineSessionAuthority(ctx context.Context, cfg config.Config, id session.ID, run func(*orchestrationate.Store, *runtimestore.Database) error) error {
	if id == "" {
		return errors.New("offline authority operation requires a session")
	}
	resolved, err := cfg.Resolve()
	if err != nil {
		return err
	}
	return withOfflineDatabase(ctx, resolved, func(database *runtimestore.Database) (result error) {
		holder := &App{config: resolved}
		defer func() {
			for _, lock := range holder.sessionOwners {
				result = errors.Join(result, lock.Close())
			}
		}()
		pending := &offlineSessionPending{authorityPendingStore: database.ATERequests(), owner: holder, id: id, preexisting: make(map[api.RequestKey]bool)}
		var after *api.RequestKey
		for {
			records, err := pending.List(ctx, aterequest.ListOptions{After: after, Limit: aterequest.MaxListRecords, MaxBytes: aterequest.MaxListBytes})
			if err != nil {
				return err
			}
			if len(records) == 0 {
				break
			}
			for _, record := range records {
				pending.preexisting[record.Request.Key()] = true
			}
			key := records[len(records)-1].Request.Key()
			after = &key
		}
		resources, err := openAuthority(ctx, resolved, pending)
		if err != nil {
			return err
		}
		defer func() { result = errors.Join(result, resources.process.Close()) }()
		// New work is now admitted only through the owned Session's operation.
		pending.preexisting = nil
		repositories, err := orchestrationate.New(resources.records, resources.sessions)
		if err != nil {
			return err
		}
		holder.repositories = repositories
		holder.stateMu.Lock()
		err = holder.resumeOwnedSessionMaintenance(ctx, id)
		holder.stateMu.Unlock()
		if err != nil {
			return err
		}
		return run(repositories, database)
	})
}

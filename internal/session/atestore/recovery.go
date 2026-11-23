package atestore

import (
	"bytes"
	"context"
	"errors"
	"strings"

	"github.com/vkviyu/agent-temporal-engine/agentprofile"
	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

// savedAppend is owned by one Append call, independent of executor/store memory.
// It becomes eligible for fresh reconciliation only after the whole plan is saved.
type savedAppend struct {
	plan    appendPlan
	records map[api.RequestKey]aterequest.Record
	events  []session.Event
}

func (s *Store) saveAppend(ctx context.Context, plan appendPlan, events []session.Event) (*savedAppend, error) {
	var saved *savedAppend
	if s.reconciliationTimeout > 0 {
		if len(events) == 0 {
			return nil, ErrCorrupt
		}
		// Validate the final append against the original complete envelopes,
		// including the upload's descriptor, identities, sequence and bytes.
		canonical, err := s.prepareAppend(events[0].SessionID, events[0].Sequence-1, events)
		if err != nil {
			return nil, err
		}
		saved = &savedAppend{records: make(map[api.RequestKey]aterequest.Record)}
		freeze := func(request, expected api.CommitRequest) (api.CommitRequest, error) {
			record, err := aterequest.NewRecord(s.storeID, request)
			if err != nil {
				return api.CommitRequest{}, err
			}
			want, err := aterequest.NewRecord(s.storeID, expected)
			if err != nil || !aterequest.Equal(record, want) {
				return api.CommitRequest{}, errors.Join(ErrCorrupt, err)
			}
			saved.records[request.Key()] = record
			return record.Request, nil
		}
		saved.plan.Request, err = freeze(plan.Request, canonical.Request)
		if err != nil {
			return nil, err
		}
		if plan.Upload != nil {
			if canonical.Upload == nil {
				return nil, ErrCorrupt
			}
			upload, err := freeze(*plan.Upload, *canonical.Upload)
			if err != nil {
				return nil, err
			}
			saved.plan.Upload = &upload
		}
		saved.events = append([]session.Event(nil), events...)
		for i := range saved.events {
			saved.events[i].Payload = bytes.Clone(events[i].Payload)
		}
	}
	if err := s.savePlan(ctx, plan); err != nil {
		return nil, err
	}
	return saved, nil
}

func (s *Store) reconcileAppend(original context.Context, saved *savedAppend) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(original), s.reconciliationTimeout)
	defer cancel()
	return s.withFreshClient(ctx, func(ctx context.Context, c *client.Client) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.gate:
		}
		defer func() { s.gate <- struct{}{} }()
		pending := reconciliationPending{PendingStore: s.pending, records: saved.records}
		// Validate the entire original plan before acknowledging any dependency.
		// Never scan/recover a different pending request or recalculate a head.
		for key := range saved.records {
			if _, _, err := pending.Get(ctx, key); err != nil {
				return err
			}
		}
		executor, err := aterequest.NewExecutor(c, pending, s.storeID)
		if err != nil {
			return err
		}
		resumer := savedExecutor{executor}
		if err := s.executeUpload(ctx, resumer, saved.plan); err != nil {
			return err
		}
		return s.executeAppend(ctx, c, resumer, saved.plan.Request)
	})
}

// Execute inserts a missing row; Resume does not. Reuse the normal ACK and
// rejection checks without ever granting recovery permission to insert a plan.
type savedExecutor struct{ *aterequest.Executor }

func (e savedExecutor) Execute(ctx context.Context, request api.CommitRequest) (api.CommitResult, error) {
	return e.Resume(ctx, request.Key())
}

type reconciliationPending struct {
	aterequest.PendingStore
	records map[api.RequestKey]aterequest.Record
}

func (p reconciliationPending) Get(ctx context.Context, key api.RequestKey) (aterequest.Record, bool, error) {
	original, ok := p.records[key]
	if !ok {
		return aterequest.Record{}, false, ErrIdentity
	}
	record, found, err := p.PendingStore.Get(ctx, key)
	if err != nil {
		return aterequest.Record{}, false, err
	}
	if found {
		if err := record.Validate(); err != nil {
			return aterequest.Record{}, false, err
		}
		if !aterequest.Equal(record, original) {
			return aterequest.Record{}, false, aterequest.ErrConflict
		}
		return record, true, nil
	}
	// Concurrent cleanup is not proof of success. A synthetic attempted record
	// allows verified native receipt lookup only; it is never persisted, and
	// absence cannot acquire an attempt or resurrect the missing envelope.
	original.Attempted = true
	return original, true, nil
}

// OwnsPendingRequest identifies records whose cleanup must remain plan-aware.
// Startup must leave these records together, including acknowledged uploads:
// an upload can contain the only saved copy of the final append descriptor.
func OwnsPendingRequest(request api.CommitRequest) bool {
	return request.Principal == "gsea" && strings.HasPrefix(string(request.RequestID), requestPrefix)
}

// RecoverSession resumes only this Session's saved native commands. The caller
// must hold its execution-owner lease for the entire call, before loading any
// runtime history. No fresh event identity or timestamp is generated.
func (s *Store) RecoverSession(ctx context.Context, id session.ID) error {
	if err := checkID(id); err != nil {
		return err
	}
	return s.withFreshClient(ctx, func(ctx context.Context, c *client.Client) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.gate:
		}
		defer func() { s.gate <- struct{}{} }()
		executor, err := s.newExecutor(c, s.storeID)
		if err != nil {
			return err
		}
		if nilValue(executor) {
			return ErrIdentity
		}
		plan, err := s.pendingAppend(ctx, executor, id)
		if err != nil || plan == nil {
			return err
		}
		if retired, err := s.retireRejectedDependency(ctx, *plan); err != nil {
			return err
		} else if retired {
			return aterequest.ErrRejected
		}
		if _, err := s.recoverEvents(ctx, c, *plan); err != nil {
			return err
		}
		// Materialize the final request before acknowledging/removing the
		// upload, including recovery between the two original local inserts.
		if err := s.savePlan(ctx, *plan); err != nil {
			return err
		}
		if err := s.executeUpload(ctx, executor, *plan); err != nil {
			return err
		}
		return s.executeAppend(ctx, c, executor, plan.Request)
	})
}

func (s *Store) executeUpload(ctx context.Context, executor Executor, plan appendPlan) error {
	if plan.Upload == nil {
		return nil
	}
	err := s.execute(ctx, executor, *plan.Upload)
	if !errors.Is(err, aterequest.ErrRejected) {
		return err
	}
	_, retireErr := s.retireRejectedDependency(ctx, plan)
	return errors.Join(err, retireErr)
}

// Only an actual persisted terminal rejection of the paired upload permits
// abandoning its dependent append. Missing chunks or missing receipts do not.
func (s *Store) retireRejectedDependency(ctx context.Context, plan appendPlan) (bool, error) {
	store, ok := s.pending.(aterequest.RetirementStore)
	if !ok {
		return false, nil
	}
	nonce, ok := strings.CutPrefix(string(plan.Request.RequestID), requestPrefix+"append-")
	if !ok || nonce == "" {
		return false, nil
	}
	key := plan.Request.Key()
	key.RequestID = model.RequestID(requestPrefix + "upload-" + nonce)
	terminal, found, err := store.GetTerminal(ctx, key)
	if err != nil || !found {
		return false, err
	}
	if terminal.StoreID != s.storeID || terminal.Key != key || terminal.Retired {
		return false, ErrCorrupt
	}
	if _, err := aterequest.EncodeRejection(terminal); err != nil {
		return false, err
	}
	if plan.Upload != nil {
		record, err := aterequest.NewRecord(s.storeID, *plan.Upload)
		if err != nil || record.Digest != terminal.Digest {
			return false, errors.Join(ErrCorrupt, err)
		}
		// Finish a prior rejection whose terminal fence was durable but whose
		// payload deletion failed. This cannot send the upload.
		if err := s.pending.DeleteRejected(ctx, record, terminal.Result); err != nil {
			return false, err
		}
	}
	command, err := agentprofile.DecodeCommand(plan.Request.Body)
	if err != nil || command.AppendSessionEvents == nil {
		return false, errors.Join(ErrCorrupt, err)
	}
	for _, event := range command.AppendSessionEvents.Events {
		if !event.PayloadRef.Present() {
			return false, ErrCorrupt
		}
		for _, chunk := range event.PayloadRef.Chunks {
			if !strings.HasPrefix(string(chunk.Key), "gsea/session-payload/"+nonce+"/") {
				return false, ErrCorrupt
			}
		}
	}
	record, err := aterequest.NewRecord(s.storeID, plan.Request)
	if err != nil {
		return false, err
	}
	saved, found, err := s.pending.Get(ctx, plan.Request.Key())
	if err != nil {
		return false, err
	}
	if found && saved.Attempted {
		// A legacy recovery may have attempted the append out of order.
		// Rejecting its upload does not resolve that append's outcome.
		return false, errors.Join(ErrPending, aterequest.ErrUnknown)
	}
	if err := store.RetireUnattempted(ctx, record); err != nil {
		return false, err
	}
	return true, nil
}

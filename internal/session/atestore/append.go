package atestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/vkviyu/agent-temporal-engine/agentprofile"
	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

const requestPrefix = "gsea-session-"

// Append commits the entire logical batch once. AnySequence captures the native
// sequence once and still submits a CAS; it never retries against a newer head.
// Retrying an unresolved append with the same arguments recovers its saved
// envelope, including after reopening with the same transient PendingStore.
// A durable receipt recovers success; ambiguous absence stays ErrUnknown.
// When ReconciliationTimeout is enabled, an unknown outcome or deadline after
// saving the plan gets one fresh lease to resume only those exact requests.
func (s *Store) Append(ctx context.Context, id session.ID, expected uint64, pending ...session.PendingEvent) ([]session.Event, error) {
	if err := checkID(id); err != nil {
		return nil, err
	}
	if ctx == nil {
		return nil, errors.New("atestore: context required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(pending) > agentprofile.MaxSessionAppendEvents {
		return nil, fmt.Errorf("%w: at most %d events per atomic append", ErrLimit, agentprofile.MaxSessionAppendEvents)
	}
	if len(pending) == 0 {
		return []session.Event{}, nil
	}
	owned := make([]session.PendingEvent, len(pending))
	payloadBytes := 0
	for i, p := range pending {
		if len(p.Payload) > MaxAppendBytes {
			return nil, ErrEventTooLarge
		}
		payloadBytes += len(p.Payload)
		if payloadBytes > MaxAppendBytes {
			return nil, ErrLimit
		}
		if err := p.Validate(); err != nil {
			return nil, err
		}
		if !validText(string(p.Type)) || !optionalText(p.CausationID) ||
			(p.CorrelationID != "" && !validIdentity(p.CorrelationID)) ||
			!optionalText(p.RuntimeSnapshotID) || !utf8.Valid(p.Payload) {
			return nil, session.ErrInvalidEvent
		}
		owned[i] = p
		owned[i].Payload = bytes.Clone(p.Payload)
	}
	results := make(chan []session.Event, 1)
	var saved *savedAppend
	err := s.withClient(ctx, func(ctx context.Context, c *client.Client) error {
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
			return errors.New("atestore: nil executor")
		}
		original, err := s.pendingAppend(ctx, executor, id)
		if err != nil {
			return err
		}
		if original != nil {
			if retired, err := s.retireRejectedDependency(ctx, *original); err != nil {
				return err
			} else if retired {
				return aterequest.ErrRejected
			}
			command, err := agentprofile.DecodeCommand(original.Request.Body)
			if err != nil {
				return err
			}
			appendCommand := command.AppendSessionEvents
			events, err := s.recoverEvents(ctx, c, *original)
			if err != nil {
				return err
			}
			if (expected != session.AnySequence && expected != appendCommand.ExpectedSequence) ||
				!samePending(events, owned) {
				return ErrPending
			}
			saved, err = s.saveAppend(ctx, *original, events)
			if err != nil {
				return err
			}
			if err := s.executeUpload(ctx, executor, *original); err != nil {
				return err
			}
			if err := s.executeAppend(ctx, c, executor, original.Request); err != nil {
				return err
			}
			results <- events
			return nil
		}
		native, found, err := s.nativeSession(ctx, c, id)
		if err != nil {
			return err
		}
		current := native.Sequence
		if expected != session.AnySequence && current != expected {
			return fmt.Errorf("%w: expected %d, current %d", session.ErrSequenceConflict, expected, current)
		}
		if uint64(len(owned)) > math.MaxUint64-current {
			return ErrLimit
		}
		events, err := makeEvents(id, current, owned)
		if err != nil {
			return err
		}
		plan, err := s.prepareAppend(id, current, events)
		if err != nil {
			return err
		}
		// Complete encoded size validation precedes even empty-session creation.
		if !found {
			create, err := s.createRequest(id)
			if err != nil {
				return err
			}
			if err := s.execute(ctx, executor, create); err != nil {
				return err
			}
		}
		saved, err = s.saveAppend(ctx, plan, events)
		if err != nil {
			return err
		}
		if err := s.executeUpload(ctx, executor, plan); err != nil {
			return err
		}
		if err := s.executeAppend(ctx, c, executor, plan.Request); err != nil {
			return err
		}
		results <- events
		return nil
	})
	select {
	case events := <-results:
		// A verified durable acknowledgement already completed. A later child
		// shutdown failure must not turn this into a fresh AnySequence retry.
		return events, nil
	default:
	}
	// withClient has joined the callback and the original process lease has
	// unwound. Only now may we inspect its captured plan or acquire a new lease.
	if saved != nil && !aterequest.ReconciliationDeferred(ctx) &&
		!errors.Is(err, aterequest.ErrCleanup) && !errors.Is(err, aterequest.ErrRejected) &&
		(errors.Is(err, aterequest.ErrUnknown) || errors.Is(err, context.DeadlineExceeded)) {
		if recoveryErr := s.reconcileAppend(ctx, saved); recoveryErr == nil {
			return saved.events, nil
		} else {
			return nil, errors.Join(err, recoveryErr)
		}
	}
	if err != nil {
		return nil, err
	}
	return nil, errors.New("atestore: lease did not complete append")
}

func samePending(events []session.Event, pending []session.PendingEvent) bool {
	if len(events) != len(pending) {
		return false
	}
	for i, e := range events {
		p := pending[i]
		if e.Type != p.Type || e.CausationID != p.CausationID ||
			e.CorrelationID != p.CorrelationID || e.RuntimeSnapshotID != p.RuntimeSnapshotID ||
			!bytes.Equal(e.Payload, p.Payload) {
			return false
		}
	}
	return true
}

func (s *Store) pendingRecords(ctx context.Context) ([]aterequest.Record, error) {
	records, err := aterequest.ListPending(ctx, s.pending)
	if errors.Is(err, aterequest.ErrLimit) {
		return nil, errors.Join(ErrLimit, err)
	}
	return records, err
}

// pendingAppend requires a complete bounded scan before assigning identities.
func (s *Store) pendingAppend(ctx context.Context, executor Executor, id session.ID) (*appendPlan, error) {
	records, err := s.pendingRecords(ctx)
	if err != nil {
		return nil, err
	}
	var original *appendPlan
	merge := func(plan appendPlan) error {
		if original == nil {
			original = &plan
			return nil
		}
		if !sameRequest(original.Request, plan.Request) {
			return ErrPending
		}
		if plan.Upload != nil {
			if original.Upload != nil && !sameRequest(*original.Upload, *plan.Upload) {
				return ErrCorrupt
			}
			original.Upload = plan.Upload
		}
		return nil
	}
	for _, record := range records {
		if err := record.Validate(); err != nil {
			return nil, err
		}
		r := record.Request
		if r.Space != s.space || r.Principal != "gsea" ||
			!strings.HasPrefix(string(r.RequestID), requestPrefix) {
			continue
		}
		if record.StoreID != s.storeID {
			return nil, ErrIdentity
		}
		if r.Method == api.MethodMutateState && strings.HasPrefix(string(r.RequestID), requestPrefix+"upload-") {
			plan, events, err := s.uploadPlan(r)
			if err != nil {
				return nil, err
			}
			if events[0].SessionID == id {
				if err := merge(plan); err != nil {
					return nil, err
				}
			}
			continue
		}
		if r.Method != api.MethodApplyAgent {
			continue
		}
		command, err := agentprofile.DecodeCommand(r.Body)
		if err != nil {
			return nil, err
		}
		if command.CreateSession != nil && command.CreateSession.ID == string(id) {
			if err := s.execute(ctx, executor, r); err != nil {
				return nil, err
			}
		}
		if command.AppendSessionEvents == nil || command.AppendSessionEvents.Session != string(id) {
			continue
		}
		copy := r
		copy.Body = bytes.Clone(r.Body)
		if err := merge(appendPlan{Request: copy}); err != nil {
			return nil, err
		}
	}
	return original, nil
}

func (s *Store) appendRequest(id session.ID, expected uint64, events []session.Event) (api.CommitRequest, error) {
	plan, err := s.prepareAppend(id, expected, events)
	if err != nil {
		return api.CommitRequest{}, err
	}
	if plan.Upload != nil {
		return api.CommitRequest{}, ErrPayloadReference
	}
	return plan.Request, nil
}

func (s *Store) createRequest(id session.ID) (api.CommitRequest, error) {
	raw, err := agentprofile.EncodeCommand(agentprofile.Command{CreateSession: &agentprofile.Session{
		ID: string(id), Space: s.space, Owner: "gsea", Revision: 1,
	}})
	if err != nil {
		return api.CommitRequest{}, err
	}
	sum := sha256.Sum256(raw)
	return api.CommitRequest{Space: s.space, Principal: "gsea",
		RequestID: model.RequestID(requestPrefix + "create-" + hex.EncodeToString(sum[:])),
		Method:    api.MethodApplyAgent, Body: raw}, nil
}

func (s *Store) commandEvents(command *agentprofile.AppendSessionEventsCommand) ([]session.Event, error) {
	if command == nil {
		return nil, ErrCorrupt
	}
	events := make([]session.Event, len(command.Events))
	for i, event := range command.Events {
		decoded, err := decodeEvent(agentprofile.SessionEvent{Session: command.Session, Space: s.space,
			Owner: "gsea", Sequence: command.ExpectedSequence + uint64(i) + 1, Event: event})
		if err != nil {
			return nil, err
		}
		events[i] = decoded
	}
	return events, nil
}

func (s *Store) execute(ctx context.Context, executor Executor, request api.CommitRequest) error {
	result, err := executor.Execute(ctx, request)
	if result.Code == api.CodeDurableAck {
		record, recordErr := aterequest.NewRecord(s.storeID, request)
		if recordErr != nil {
			return recordErr
		}
		if verifyErr := aterequest.VerifyAcknowledgement(record, result); verifyErr != nil {
			return verifyErr
		}
		// A cleanup failure remains visible. The next identical Append recovers
		// the acknowledgement and original events without submitting new ones.
		return err
	}
	if result.Code == api.CodeConflict && result.Conflict == api.ConflictPrecondition {
		return errors.Join(session.ErrSequenceConflict, err)
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("atestore: commit returned %s", result.Code)
}

func (s *Store) executeAppend(ctx context.Context, reader NativeReader, executor Executor, request api.CommitRequest) error {
	err := s.execute(ctx, executor, request)
	if !errors.Is(err, aterequest.ErrRejected) {
		return err
	}
	// ATE transports some profile CAS failures as a rejected command. Check
	// native sequence only after a definite rejection; never infer absence
	// from a current sequence following an unknown outcome.
	command, decodeErr := agentprofile.DecodeCommand(request.Body)
	if decodeErr != nil || command.AppendSessionEvents == nil {
		return err
	}
	appendCommand := command.AppendSessionEvents
	state, readErr := s.getSession(ctx, reader, session.ID(appendCommand.Session))
	if readErr == nil && state.Found && state.Session.Sequence != appendCommand.ExpectedSequence {
		return errors.Join(session.ErrSequenceConflict, err)
	}
	return err
}

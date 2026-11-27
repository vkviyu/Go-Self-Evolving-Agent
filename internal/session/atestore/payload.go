// Native payload staging and verified readback, with no local history.
package atestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/vkviyu/agent-temporal-engine/agentprofile"
	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

type appendPlan struct {
	Request api.CommitRequest
	Upload  *api.CommitRequest
}

// prepareAppend constructs both exact requests without touching either store.
// Unlike client.PutSessionPayload, the native upload is routed through the
// pending executor. A single bounded upload also saves the append descriptor,
// allowing recovery if the host stops between the two local Insert calls.
func (s *Store) prepareAppend(id session.ID, expected uint64, events []session.Event) (appendPlan, error) {
	plan := appendPlan{}
	plan.Request.Space, plan.Request.Principal = s.space, "gsea"
	plan.Request.Method = api.MethodApplyAgent
	host := make([]agentprofile.HostEvent, len(events))
	total, referenced := 0, false
	for i, event := range events {
		if event.SessionID != id || event.Sequence != expected+uint64(i)+1 {
			return plan, session.ErrInvalidEvent
		}
		value, err := encodeEvent(event)
		if err != nil {
			return plan, err
		}
		host[i] = value
		total += len(value.Payload)
		if total > MaxAppendBytes {
			return plan, ErrLimit
		}
		referenced = referenced || len(value.Payload) > agentprofile.MaxHostEventBytes
	}
	command := agentprofile.Command{AppendSessionEvents: &agentprofile.AppendSessionEventsCommand{
		Session: string(id), ExpectedSequence: expected, Events: host,
	}}
	// Derive the preparation identity from the exact inputs. A retry after a
	// crash must reuse its saved upload/append envelopes instead of adding a
	// second copy to the bounded pending inventory. Identical bytes are the
	// same ATE request, including after a durable rejection.
	nonceHash := sha256.New()
	fmt.Fprintf(nonceHash, "%s\x00%d\x00", id, expected)
	for _, event := range events {
		fmt.Fprintf(nonceHash, "%s\x00%s\x00%d\x00", event.Type, event.CorrelationID, len(event.Payload))
		nonceHash.Write(event.Payload)
	}
	nonce := hex.EncodeToString(nonceHash.Sum(nil))
	plan.Request.RequestID = model.RequestID(requestPrefix + "append-" + nonce)
	if !referenced {
		raw, err := agentprofile.EncodeCommand(command)
		if err == nil {
			plan.Request.Body = raw
			return plan, nil
		}
	}
	var operations []api.StateMutation
	for i := range host {
		raw := host[i].Payload
		sum := sha256.Sum256(raw)
		manifest := agentprofile.HostPayloadRef{Bytes: uint64(len(raw)), Digest: model.ObjectID(hex.EncodeToString(sum[:]))}
		for offset, part := 0, 0; offset < len(raw); part++ {
			end := min(offset+api.MaxStateValueBytes, len(raw))
			value := api.Value{Kind: api.ValueBytes, Data: bytes.Clone(raw[offset:end])}
			object, err := api.StateValueID(s.space, value)
			if err != nil {
				return plan, err
			}
			key := []byte(fmt.Sprintf("gsea/session-payload/%s/%02d/%03d", nonce, i, part))
			operations = append(operations, api.StateMutation{Key: key, Kind: api.MutationPut, Value: value})
			manifest.Chunks = append(manifest.Chunks, agentprofile.HostPayloadChunk{Key: key, Object: object})
			offset = end
		}
		host[i].Payload, host[i].PayloadRef = nil, manifest
	}
	raw, err := agentprofile.EncodeCommand(command)
	if err != nil {
		return plan, fmt.Errorf("%w: native append manifest: %v", ErrLimit, err)
	}
	plan.Request.Body = raw
	descriptor, err := json.Marshal(plan.Request)
	if err != nil {
		return plan, err
	}
	operations = append(operations, api.StateMutation{Key: []byte("gsea/session-payload/" + nonce + "/append"),
		Kind: api.MutationPut, Value: api.Value{Kind: api.ValueBytes, Data: descriptor}})
	body, err := api.EncodeStateBatch(api.StateBatch{Operations: operations})
	if err != nil {
		return plan, fmt.Errorf("%w: native upload batch: %v", ErrLimit, err)
	}
	plan.Upload = &api.CommitRequest{Space: s.space, Principal: "gsea",
		RequestID: model.RequestID(requestPrefix + "upload-" + nonce), Method: api.MethodMutateState, Body: body}
	return plan, nil
}

func (s *Store) savePlan(ctx context.Context, plan appendPlan) error {
	requests := []api.CommitRequest{plan.Request}
	if plan.Upload != nil {
		requests = append([]api.CommitRequest{*plan.Upload}, requests...)
	}
	existing, err := s.pendingRecords(ctx)
	if err != nil {
		return err
	}
	known := make(map[api.RequestKey]aterequest.Record, len(existing))
	count, size := len(existing), 0
	for _, record := range existing {
		raw, err := aterequest.EncodeRecord(record)
		if err != nil {
			return err
		}
		size += len(raw)
		known[record.Request.Key()] = record
	}
	records := make([]aterequest.Record, len(requests))
	for i, request := range requests {
		record, err := aterequest.NewRecord(s.storeID, request)
		if err != nil {
			return err
		}
		records[i] = record
		if previous, found := known[request.Key()]; found {
			if !aterequest.Equal(previous, record) {
				return aterequest.ErrConflict
			}
			continue
		}
		raw, err := aterequest.EncodeRecord(record)
		if err != nil {
			return err
		}
		count++
		size += len(raw)
	}
	if count > aterequest.MaxListRecords || size > aterequest.MaxListBytes {
		return ErrLimit
	}
	// Reserve capacity for the whole preparation before either insertion.
	// Process serialization excludes another writer sharing this store here.
	for _, record := range records {
		if _, found := known[record.Request.Key()]; found {
			// Already compared above. Do not reinsert a rejected envelope whose
			// persisted receipt still needs local-only cleanup.
			continue
		}
		if err := s.pending.Insert(ctx, record); err != nil {
			return err
		}
	}
	return nil
}

// uploadPlan authenticates the entire local preparation by reconstructing its
// canonical native commands. It never treats arbitrary pending state as history.
func (s *Store) uploadPlan(request api.CommitRequest) (appendPlan, []session.Event, error) {
	batch, err := api.DecodeStateBatch(request.Body)
	if err != nil {
		return appendPlan{}, nil, err
	}
	nonce := strings.TrimPrefix(string(request.RequestID), requestPrefix+"upload-")
	values := make(map[string]api.Value, len(batch.Operations))
	for _, op := range batch.Operations {
		values[string(op.Key)] = op.Value
	}
	descriptor := values["gsea/session-payload/"+nonce+"/append"]
	var appendRequest api.CommitRequest
	if descriptor.Kind != api.ValueBytes || json.Unmarshal(descriptor.Data, &appendRequest) != nil {
		return appendPlan{}, nil, ErrCorrupt
	}
	events, err := s.eventsFromRequest(appendRequest, func(chunk agentprofile.HostPayloadChunk) ([]byte, error) {
		value, found := values[string(chunk.Key)]
		id, err := api.StateValueID(s.space, value)
		if !found || err != nil || value.Kind != api.ValueBytes || id != chunk.Object {
			return nil, ErrCorrupt
		}
		return value.Data, nil
	})
	if err != nil || len(events) == 0 {
		return appendPlan{}, nil, errors.Join(ErrCorrupt, err)
	}
	plan, err := s.prepareAppend(events[0].SessionID, events[0].Sequence-1, events)
	if err != nil || plan.Upload == nil || !sameRequest(*plan.Upload, request) || !sameRequest(plan.Request, appendRequest) {
		return appendPlan{}, nil, errors.Join(ErrCorrupt, err)
	}
	return plan, events, nil
}

func sameRequest(a, b api.CommitRequest) bool {
	left, err := api.CommandDigest(a)
	right, other := api.CommandDigest(b)
	return err == nil && other == nil && left == right
}

func (s *Store) eventsFromRequest(request api.CommitRequest, chunk func(agentprofile.HostPayloadChunk) ([]byte, error)) ([]session.Event, error) {
	if request.Space != s.space || request.Principal != "gsea" || request.Method != api.MethodApplyAgent {
		return nil, ErrIdentity
	}
	command, err := agentprofile.DecodeCommand(request.Body)
	if err != nil || command.AppendSessionEvents == nil {
		return nil, errors.Join(ErrCorrupt, err)
	}
	v := command.AppendSessionEvents
	events := make([]session.Event, len(v.Events))
	total := 0
	for i, host := range v.Events {
		size, err := envelopeSize(host)
		if err != nil {
			return nil, err
		}
		if size > MaxAppendBytes-total {
			return nil, ErrLimit
		}
		total += size
		raw := host.Payload
		if host.PayloadRef.Present() {
			if chunk == nil {
				return nil, ErrPayloadReference
			}
			var buffer boundedPayload
			buffer.remaining = size
			for _, part := range host.PayloadRef.Chunks {
				data, err := chunk(part)
				if err != nil {
					return nil, err
				}
				if _, err := buffer.Write(data); err != nil {
					return nil, err
				}
			}
			raw = buffer.Bytes()
			if err := verifyPayload(host.PayloadRef, raw); err != nil {
				return nil, err
			}
		}
		events[i], err = decodeEnvelope(agentprofile.SessionEvent{Session: v.Session, Space: s.space,
			Owner: "gsea", Sequence: v.ExpectedSequence + uint64(i) + 1, Event: host}, raw)
		if err != nil {
			return nil, err
		}
	}
	return events, nil
}

func (s *Store) recoverEvents(ctx context.Context, c *client.Client, plan appendPlan) (events []session.Event, err error) {
	if plan.Upload != nil {
		_, events, err := s.uploadPlan(*plan.Upload)
		return events, err
	}
	// Uploaded chunks are already authoritative native values, even when the
	// SessionEvent has not yet been published. Read them at one fixed view.
	view, err := c.OpenView(ctx, api.OpenViewRequest{Space: s.space, Principal: "gsea", Mode: "state"})
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, closeView(ctx, c, view.Ref))
		if err != nil {
			events = nil
		}
	}()
	if view.Anchor.Validate() != nil || view.Anchor.Space != s.space ||
		view.Anchor.Fence.Store != s.storeID || view.Anchor.Cursor != nil || view.Anchor.Knowledge != nil {
		return nil, ErrIdentity
	}
	return s.eventsFromRequest(plan.Request, func(part agentprofile.HostPayloadChunk) ([]byte, error) {
		result, err := c.GetViewState(ctx, api.ReadViewStateRequest{View: view.Ref, Key: part.Key})
		if err != nil {
			return nil, err
		}
		if result.Fence != view.Anchor.Fence {
			return nil, ErrIdentity
		}
		if !result.Found {
			return nil, fmt.Errorf("%w: native upload is unavailable; dependent append cannot be submitted", ErrPending)
		}
		id, err := api.StateValueID(s.space, result.Value)
		if err != nil || result.Value.Kind != api.ValueBytes || id != part.Object {
			return nil, ErrCorrupt
		}
		return result.Value.Data, nil
	})
}

type boundedPayload struct {
	buffer    bytes.Buffer
	remaining int
}

// Bytes returns the accumulated payload bytes backed by the buffer.
func (b *boundedPayload) Bytes() []byte { return b.buffer.Bytes() }

func (b *boundedPayload) Write(data []byte) (int, error) {
	if len(data) > b.remaining {
		return 0, ErrLimit
	}
	b.remaining -= len(data)
	return b.buffer.Write(data)
}

func envelopeSize(host agentprofile.HostEvent) (int, error) {
	if !host.PayloadRef.Present() {
		if len(host.Payload) == 0 || len(host.Payload) > agentprofile.MaxHostEventBytes {
			return 0, ErrCorrupt
		}
		return len(host.Payload), nil
	}
	if len(host.Payload) != 0 || host.PayloadRef.Validate() != nil {
		return 0, ErrCorrupt
	}
	if host.PayloadRef.Bytes > MaxReadBytes {
		return 0, ErrLimit
	}
	return int(host.PayloadRef.Bytes), nil
}

func verifyPayload(ref agentprofile.HostPayloadRef, raw []byte) error {
	sum := sha256.Sum256(raw)
	if uint64(len(raw)) != ref.Bytes || hex.EncodeToString(sum[:]) != string(ref.Digest) {
		return ErrCorrupt
	}
	return nil
}

type payloadReader interface {
	ReadSessionEventPayload(context.Context, api.SessionEventItem, io.Writer) error
}

type resolvedItem struct {
	api.SessionEventItem
	envelope []byte
	decoded  session.Event
}

func resolveItem(ctx context.Context, reader payloadReader, item api.SessionEventItem, budget int) (resolvedItem, error) {
	size, err := envelopeSize(item.Event.Event)
	if err != nil {
		return resolvedItem{}, err
	}
	if size > budget {
		return resolvedItem{}, ErrLimit
	}
	raw := item.Event.Event.Payload
	if item.Event.Event.PayloadRef.Present() {
		buffer := &boundedPayload{remaining: size}
		if err := reader.ReadSessionEventPayload(ctx, item, buffer); err != nil {
			return resolvedItem{}, err
		}
		raw = buffer.Bytes()
		if err := verifyPayload(item.Event.Event.PayloadRef, raw); err != nil {
			return resolvedItem{}, err
		}
	}
	event, err := decodeEnvelope(item.Event, raw)
	if err != nil {
		return resolvedItem{}, err
	}
	return resolvedItem{SessionEventItem: item, envelope: raw, decoded: event}, nil
}

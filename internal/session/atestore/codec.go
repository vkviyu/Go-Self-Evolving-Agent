package atestore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/vkviyu/agent-temporal-engine/agentprofile"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

const (
	EventSchema        = "gsea/session-event"
	EventSchemaVersion = 1
	EventEncoding      = "application/json"
)

func hostKind(kind session.EventType) agentprofile.HostEventKind {
	switch kind {
	case session.EventUserMessage, session.EventBackgroundInput, session.EventAssistantMessage:
		return agentprofile.HostEventMessage
	case session.EventModelRequested:
		return agentprofile.HostEventModelRequest
	case session.EventModelPartial, session.EventModelFailed:
		return agentprofile.HostEventModelResponse
	case session.EventToolProposed, session.EventToolStarted:
		return agentprofile.HostEventToolRequest
	case session.EventToolSucceeded, session.EventToolFailed, session.EventToolSuspended, session.EventToolOutcomeUnknown:
		return agentprofile.HostEventToolResponse
	default:
		return agentprofile.HostEventHost
	}
}

func validateEvent(event session.Event) error {
	if !validText(string(event.SessionID)) || !validText(event.EventID) ||
		!validText(string(event.Type)) || event.Sequence == 0 ||
		event.OccurredAt.IsZero() || event.SchemaVersion != EventSchemaVersion ||
		!optionalText(event.CausationID) || !optionalText(event.CorrelationID) ||
		!optionalText(event.RuntimeSnapshotID) || !json.Valid(event.Payload) ||
		!utf8.Valid(event.Payload) {
		return session.ErrInvalidEvent
	}
	checksum, err := session.EventChecksum(event)
	if err != nil || checksum != event.Checksum {
		return fmt.Errorf("%w: envelope checksum", ErrCorrupt)
	}
	return nil
}

// encodeEvent inserts the raw JSON value without json.Marshal's compaction or
// HTML escaping. Even Payload framing whitespace and original number spelling
// survive. ATE persists this complete original envelope verbatim.
func encodeEvent(event session.Event) (agentprofile.HostEvent, error) {
	if err := validateEvent(event); err != nil {
		return agentprofile.HostEvent{}, err
	}
	payload := event.Payload
	event.Payload = nil
	raw, err := json.Marshal(event)
	if err != nil {
		return agentprofile.HostEvent{}, err
	}
	raw = bytes.Replace(raw, []byte(`"Payload":null`), append([]byte(`"Payload":`), payload...), 1)
	if len(raw) > MaxAppendBytes {
		return agentprofile.HostEvent{}, fmt.Errorf("%w: envelope is %d bytes; append maximum is %d", ErrEventTooLarge, len(raw), MaxAppendBytes)
	}
	return agentprofile.HostEvent{
		Kind: hostKind(event.Type), Schema: EventSchema, SchemaVersion: EventSchemaVersion,
		Encoding: EventEncoding, CorrelationID: event.CorrelationID, Payload: raw,
	}, nil
}

func decodeEvent(native agentprofile.SessionEvent) (session.Event, error) {
	if native.Event.PayloadRef.Present() {
		return session.Event{}, ErrPayloadReference
	}
	return decodeEnvelope(native, native.Event.Payload)
}

// decodeEnvelope accepts only the complete, verified bytes of an inline event
// or its native manifest. Canonical checks preserve the raw Payload bytes.
func decodeEnvelope(native agentprofile.SessionEvent, raw []byte) (session.Event, error) {
	host := native.Event
	size, err := envelopeSize(host)
	if err != nil {
		return session.Event{}, err
	}
	if host.Schema != EventSchema || host.SchemaVersion != EventSchemaVersion ||
		host.Encoding != EventEncoding || len(raw) != size {
		return session.Event{}, fmt.Errorf("%w: unsupported host envelope", ErrCorrupt)
	}
	if host.PayloadRef.Present() {
		if err := verifyPayload(host.PayloadRef, raw); err != nil {
			return session.Event{}, err
		}
	} else if !bytes.Equal(raw, host.Payload) {
		return session.Event{}, ErrCorrupt
	}
	host.Payload = raw
	var event session.Event
	if err := json.Unmarshal(host.Payload, &event); err != nil {
		return session.Event{}, fmt.Errorf("%w: event JSON: %v", ErrCorrupt, err)
	}
	// RawMessage unmarshalling discards whitespace around its JSON value.
	// Recover those bytes between the deterministic envelope prefix/suffix.
	event.Payload = nil
	stub, err := json.Marshal(event)
	if err != nil {
		return session.Event{}, ErrCorrupt
	}
	prefix, suffix, found := bytes.Cut(stub, []byte(`"Payload":null`))
	prefix = append(prefix, []byte(`"Payload":`)...)
	if !found || len(host.Payload) < len(prefix)+len(suffix) ||
		!bytes.HasPrefix(host.Payload, prefix) || !bytes.HasSuffix(host.Payload, suffix) {
		return session.Event{}, ErrCorrupt
	}
	event.Payload = bytes.Clone(host.Payload[len(prefix) : len(host.Payload)-len(suffix)])
	if err := validateEvent(event); err != nil {
		return session.Event{}, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if string(event.SessionID) != native.Session || event.Sequence != native.Sequence ||
		host.CorrelationID != event.CorrelationID || host.Kind != hostKind(event.Type) {
		return session.Event{}, fmt.Errorf("%w: native and original identities differ", ErrCorrupt)
	}
	// Reject duplicate fields, unknown fields and noncanonical host envelopes.
	// The raw original Payload value itself is deliberately not canonicalized.
	encoded, err := encodeEvent(event)
	if err != nil || !bytes.Equal(encoded.Payload, host.Payload) {
		return session.Event{}, fmt.Errorf("%w: noncanonical event envelope", ErrCorrupt)
	}
	return event, nil
}

// validText checks that a required session envelope field is nonempty, size-bounded UTF-8.
func validText(s string) bool {
	return s != "" && len(s) <= 1024 && utf8.ValidString(s)
}

// optionalText accepts an empty envelope field or one satisfying the required text
// constraints.
func optionalText(s string) bool { return s == "" || validText(s) }

func makeEvents(id session.ID, sequence uint64, pending []session.PendingEvent) ([]session.Event, error) {
	now := time.Now().UTC()
	events := make([]session.Event, len(pending))
	for i, p := range pending {
		eventID, err := randomID()
		if err != nil {
			return nil, err
		}
		event := session.Event{
			SessionID: id, Sequence: sequence + uint64(i) + 1, EventID: eventID,
			Type: p.Type, OccurredAt: now.Add(time.Duration(i) * time.Nanosecond),
			CausationID: p.CausationID, CorrelationID: p.CorrelationID,
			RuntimeSnapshotID: p.RuntimeSnapshotID,
			Payload:           bytes.Clone(p.Payload), SchemaVersion: EventSchemaVersion,
		}
		event.Checksum, err = session.EventChecksum(event)
		if err != nil {
			return nil, err
		}
		events[i] = event
	}
	return events, nil
}

package experience

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateports"
	"github.com/vkviyu/go-self-evolving-agent/internal/experienceport"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

const primaryPolicy = "primary-session-evidence/v2"

type (
	HistoryCoverage = experienceport.HistoryCoverage
	historyCut      struct {
		Through   uint64
		Binding   string
		Saturated bool
	}
	evidenceEvent struct {
		SourceID       string
		Sequence       uint64
		Kind           string
		Payload        json.RawMessage
		OriginalSHA256 string
		Projected      bool
	}
	EvidenceReference = experienceport.EvidenceReference
)

// payloadDigest hashes the exact source payload bytes used as evidence.
func payloadDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// The journal envelope is session.Event, not the inner runtime payload.
// Purpose is trusted routing metadata only at that exact envelope location;
// words or JSON quoted inside a user's content never change its event kind.
func projectEvidence(r ateports.Record) (evidenceEvent, string, error) {
	var envelope session.Event
	if err := json.Unmarshal(r.Payload, &envelope); err != nil {
		return evidenceEvent{}, "", err
	}
	if envelope.EventID != r.SourceID || envelope.Sequence != r.Sequence || string(envelope.SessionID) != r.SessionID || string(envelope.Type) != r.Kind {
		return evidenceEvent{}, "", fmt.Errorf("%w: original event envelope mismatch", ErrDecision)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Payload, &fields); err != nil || fields == nil {
		return evidenceEvent{}, "", fmt.Errorf("%w: event payload must be an object", ErrDecision)
	}
	var purpose string
	if raw, ok := fields["purpose"]; ok {
		if err := json.Unmarshal(raw, &purpose); err != nil {
			return evidenceEvent{}, "", ErrDecision
		}
	}
	kind := envelope.Type
	if (strings.HasPrefix(r.Kind, "model.") || kind == session.EventAssistantMessage) && purpose != "" {
		return evidenceEvent{}, "auxiliary_model_or_assistant", nil
	}
	event := evidenceEvent{SourceID: r.SourceID, Sequence: r.Sequence, Kind: r.Kind, Payload: envelope.Payload, OriginalSHA256: payloadDigest(r.Payload)}
	switch kind {
	case session.EventUserMessage, session.EventBackgroundInput,
		session.EventToolProposed, session.EventToolStarted, session.EventToolSuspended,
		session.EventToolSucceeded, session.EventToolFailed, session.EventToolOutcomeUnknown,
		session.EventPolicyDecided, session.EventApprovalRequested, session.EventApprovalResolved,
		session.EventModelFailed, session.EventTurnFailed, session.EventTurnAbandoned:
		// Preserve complete original primary payloads, including unknown tool
		// outcomes. These are evidence of the recorded event, not inferred success.
	case session.EventAssistantMessage:
		// The normal assistant receipt contains the entire model input and
		// previous experience context. Keep the response, never that wrapper.
		event.Payload = pickFields(fields, "content", "tool_calls", "finish_reason", "request_id", "turn_id")
		event.Projected = true
	case session.EventTurnCompleted:
		event.Payload = pickFields(fields, "content", "turn_id", "finish_reason", "status", "stop_reason")
		event.Projected = true
	case session.EventTurnStarted, session.EventTurnPrepared, session.EventModelRequested:
		return evidenceEvent{}, "repeated_prompt_or_request_wrapper", nil
	case session.EventContextCompacted, session.EventContextManaged, session.EventCheckpointCreated:
		return evidenceEvent{}, "derived_context_projection", nil
	case session.EventModelPartial:
		return evidenceEvent{}, "partial_response_projection", nil
	default:
		return evidenceEvent{}, "nonprimary_control_event", nil
	}
	return event, "", nil
}

func pickFields(fields map[string]json.RawMessage, names ...string) json.RawMessage {
	out := map[string]json.RawMessage{}
	for _, name := range names {
		if value, ok := fields[name]; ok {
			out[name] = value
		}
	}
	raw, _ := json.Marshal(out)
	return raw
}

func evidenceReferences(records []ateports.Record) []EvidenceReference {
	if len(records) == 0 {
		return nil
	}
	out := make([]EvidenceReference, 0, len(records))
	for _, record := range records {
		out = append(out, EvidenceReference{
			SourceID:       record.SourceID,
			Sequence:       record.Sequence,
			Kind:           record.Kind,
			OriginalSHA256: payloadDigest(record.Payload),
		})
	}
	return out
}

func validateToolReview(r review, events []evidenceEvent) error {
	allowed := map[string]bool{}
	cited := map[string]bool{}
	for _, id := range r.EventIDs {
		cited[id] = true
	}
	for _, e := range events {
		if e.Kind == string(session.EventToolSucceeded) && cited[e.SourceID] {
			allowed[e.SourceID] = true
		}
	}
	return citations(r.ToolSuccessEventIDs, allowed, r.Passed && r.ClaimsToolSuccess)
}

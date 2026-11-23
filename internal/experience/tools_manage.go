package experience

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/vkviyu/agent-temporal-engine/memory"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

type (
	memoryManageInput struct {
		Operation string `json:"operation"`
		Space     string `json:"space,omitempty"`
		NewSpace  *struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"new_space,omitempty"`
		Claim     string   `json:"claim,omitempty"`
		Kind      string   `json:"kind,omitempty"`
		Tags      []string `json:"tags,omitempty"`
		EventIDs  []string `json:"event_ids,omitempty"`
		Ref       string   `json:"ref,omitempty"`
		SourceRef string   `json:"source_ref,omitempty"`
		Reason    string   `json:"reason,omitempty"`
	}

	memoryToolPlan struct {
		Candidate  *memory.Memory
		Source     *memory.Source
		Retraction *memory.Retraction
		Provenance []EvidenceReference
	}

	memoryManageResult struct {
		Operation   string              `json:"operation"`
		Status      string              `json:"status"`
		Ref         string              `json:"ref,omitempty"`
		Claim       string              `json:"claim,omitempty"`
		SourceRef   string              `json:"source_ref,omitempty"`
		Provenance  []EvidenceReference `json:"provenance,omitempty"`
		Adopted     bool                `json:"adopted"`
		Execution   *model.KnowledgeKey `json:"execution,omitempty"`
		Observation model.ObjectID      `json:"observation,omitempty"`
		Note        string              `json:"note"`
	}
)

// validate enforces operation-specific arguments before persisting the request.
func (in memoryManageInput) validate() error {
	if !boundedText(in.Reason, 2048, false) || !boundedText(in.Ref, 2048, false) ||
		!boundedText(in.SourceRef, 2048, false) {
		return ErrDecision
	}
	switch in.Operation {
	case "create_candidate":
		if (in.Space == "") == (in.NewSpace == nil) || len(in.Space) > 1024 ||
			!boundedText(in.Claim, 4096, true) || !stringsValid(in.Tags, 16, 128) ||
			!stringsValid(in.EventIDs, 16, 1024) || in.Ref != "" || in.SourceRef != "" || in.Reason != "" {
			return ErrDecision
		}
		if in.NewSpace != nil && (!boundedText(in.NewSpace.Name, 512, true) ||
			!boundedText(in.NewSpace.Description, 8192, true)) {
			return ErrDecision
		}
		switch memory.Kind(in.Kind) {
		case memory.KindFact, memory.KindDecision, memory.KindProcedure, memory.KindFailurePattern, memory.KindPreference:
		default:
			return ErrDecision
		}
	case "withdraw_source", "correct_source", "adopt":
		if in.Ref == "" || in.Space != "" || in.NewSpace != nil || in.Claim != "" ||
			in.Kind != "" || len(in.Tags) != 0 || len(in.EventIDs) != 0 {
			return ErrDecision
		}
		if _, _, err := memoryToolRef(in.Ref); err != nil {
			return err
		}
		if in.Operation == "adopt" {
			if in.SourceRef != "" || in.Reason != "" {
				return ErrDecision
			}
		} else if !boundedText(in.Reason, 2048, true) {
			return ErrDecision
		}
	default:
		return errors.New("experience: operation must be create_candidate, withdraw_source, correct_source or adopt")
	}
	return nil
}

// ManageMemoryTool uses one identity for all operations, so changing arguments
// or actor on replay conflicts instead of silently creating a different write.
// The host must register this executor as RiskWrite and require approval.
func (s *Service) ManageMemoryTool(ctx context.Context, caller MemoryCaller, raw json.RawMessage) (json.RawMessage, error) {
	if err := caller.validate(); err != nil {
		return nil, err
	}
	var in memoryManageInput
	if err := decodeMemoryTool(raw, &in); err != nil {
		return nil, err
	}
	if err := in.validate(); err != nil {
		return nil, err
	}
	unlock, err := s.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	key := s.key(caller.SessionID, caller.TurnID) + "/memory-tool/" + digest(caller.CallID)
	if err := s.save(ctx, key+"/input", struct {
		Caller MemoryCaller
		Input  memoryManageInput
	}{caller, in}); err != nil {
		return nil, err
	}
	if in.Operation == "adopt" {
		return s.toolAdopt(ctx, key, caller, in.Ref)
	}
	var plan memoryToolPlan
	if found, err := s.load(ctx, key+"/plan", &plan); err != nil {
		return nil, err
	} else if !found {
		plan, err = s.buildMemoryToolPlan(ctx, key, caller, in)
		if err != nil {
			return nil, err
		}
		// Timestamps, claim bytes, and original native anchors are immutable
		// before the first candidate/retraction publication, including retries.
		if err := s.save(ctx, key+"/plan", plan); err != nil {
			return nil, err
		}
	}
	result := memoryManageResult{Operation: in.Operation}
	if plan.Candidate != nil {
		if in.NewSpace != nil {
			if err := s.applyRoute(ctx, key, route{NewSpace: &newSpace{Name: in.NewSpace.Name, Description: in.NewSpace.Description}}); err != nil {
				return nil, err
			}
		}
		if err := s.applyMemory(ctx, key+"/candidate", plan.Candidate.Space, memory.Command{PutCandidate: plan.Candidate}); err != nil {
			return nil, err
		}
		result.Status = "candidate"
		result.Ref = string(plan.Candidate.Space) + "/" + string(plan.Candidate.ID)
		result.Claim = string(plan.Candidate.Claim)
		result.Provenance = plan.Provenance
		result.Note = "Unvalidated candidate recorded. Provenance identifies original session context; it does not verify this claim. No validation, promotion or adoption performed."
	} else if plan.Source != nil && plan.Retraction != nil {
		if err := s.applyMemory(ctx, key+"/retract-source", plan.Source.Anchor.Space, memory.Command{
			RetractSource: &memory.RetractSourceCommand{Source: plan.Source.SourceID, Retraction: *plan.Retraction},
		}); err != nil {
			return nil, err
		}
		result.Status = "source_reuse_banned"
		result.SourceRef = memoryIdentity(*plan.Source)
		result.Note = "Source reuse ban recorded in its authoritative origin space. Historical records remain; no replacement claim was created or validated."
	} else {
		return nil, ErrSelection
	}
	return json.Marshal(result)
}

// buildMemoryToolPlan resolves original evidence before freezing mutation bytes.
func (s *Service) buildMemoryToolPlan(ctx context.Context, key string, caller MemoryCaller, in memoryManageInput) (memoryToolPlan, error) {
	if in.Operation != "create_candidate" {
		record, err := s.toolRead(ctx, in.Ref)
		if err != nil {
			return memoryToolPlan{}, err
		}
		source := record.Source
		if in.SourceRef != "" && in.SourceRef != record.Ref {
			found := false
			for _, candidate := range record.Sources {
				if memoryIdentity(candidate) == in.SourceRef {
					source, found = candidate, true
					break
				}
			}
			if !found {
				return memoryToolPlan{}, errors.New("experience: source_ref must be the memory itself or one of its authoritative provenance sources")
			}
		}
		kind := memory.RetractionUseBan
		if in.Operation == "correct_source" {
			kind = memory.RetractionFactualCorrection
		}
		return memoryToolPlan{Source: &source, Retraction: &memory.Retraction{
			Kind: kind, Reason: in.Reason, RecordedAt: time.Now().UTC(),
		}}, nil
	}
	target := model.SpaceID(in.Space)
	if in.NewSpace != nil {
		target = model.SpaceID("gsea-space-" + digest(key))
	} else {
		d, err := s.directory(ctx)
		if err != nil {
			return memoryToolPlan{}, err
		}
		found := false
		for _, space := range d.Spaces {
			found = found || space.ID == target && !space.Archived
		}
		if !found {
			return memoryToolPlan{}, errors.New("experience: candidate destination must be an active semantic space from directory")
		}
	}
	h, err := s.originalHistory(ctx, key+"/history", caller.SessionID)
	if err != nil {
		return memoryToolPlan{}, err
	}
	ids := map[string]bool{}
	for _, id := range in.EventIDs {
		ids[id] = true
	}
	if len(ids) == 0 {
		// The latest original input supplies attribution, never evidence of
		// execution success. Do not cite this memory tool's own proposal.
		for i := len(h.Events) - 1; i >= 0; i-- {
			if h.Events[i].Kind == string(session.EventUserMessage) || h.Events[i].Kind == string(session.EventBackgroundInput) {
				ids[h.Events[i].SourceID] = true
				break
			}
		}
	}
	if len(ids) == 0 {
		return memoryToolPlan{}, errors.New("experience: candidate needs durable session input or explicit original event_ids")
	}
	records, err := s.recordsFor(ctx, h, ids)
	if err != nil {
		return memoryToolPlan{}, err
	}
	sources, err := s.anchorRecords(ctx, records)
	if err != nil {
		return memoryToolPlan{}, err
	}
	now := time.Now().UTC()
	m := memory.Memory{
		Space: target, Kind: memory.Kind(in.Kind), Claim: []byte(in.Claim),
		Sources: sources, Status: memory.StatusCandidate, RecordedAt: now, VisibleAt: now,
		Applicability: memory.Applicability{Tags: in.Tags},
	}
	for _, src := range sources {
		m.Evidence = append(m.Evidence, memory.Evidence{Source: src, Note: "Original session provenance only; model-proposed claim has not been independently validated."})
	}
	m.ID = memory.IDFor(m)
	if err := (memory.Command{PutCandidate: &m}).Check(); err != nil {
		return memoryToolPlan{}, fmt.Errorf("experience: invalid native candidate: %w", err)
	}
	return memoryToolPlan{Candidate: &m, Provenance: evidenceReferences(records)}, nil
}

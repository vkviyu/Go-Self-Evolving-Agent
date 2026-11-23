package experience

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/memory"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateports"
)

const validationMethod = "Independent second evidence-grounded model review of original persisted events; not tool execution, a test run, or proof of success."

type reviewInput struct {
	Candidate            lesson
	OriginalEvents       []ateports.Record
	CompleteHistoryNotes notes
	Method               string
}

// Complete processes the full original persisted session cut, regardless of
// runtime compaction. It does not infer or report execution success. The host
// separately records the actual outcome through Bridge.CompleteExecution.
func (s *Service) Complete(ctx context.Context, sessionID, turnID string, generate Generate) error {
	unlock, err := s.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if err = validInput(sessionID, turnID, ""); err != nil {
		return err
	}
	key := s.key(sessionID, turnID) + "/complete"
	var done bool
	if found, err := s.load(ctx, key+"/done", &done); err != nil {
		return err
	} else if found && done {
		return nil
	}
	h, err := s.originalHistory(ctx, key+"/history", sessionID)
	if err != nil {
		return err
	}
	summary, err := s.summarize(ctx, key+"/history", h, generate)
	if err != nil {
		return err
	}
	published, err := s.publishedLessons(ctx, sessionID)
	if err != nil {
		return err
	}
	allowed := noteIDs(summary.Notes)
	var extracted extraction
	err = s.decide(ctx, key+"/extract", "extract",
		`{"lessons":[{"claim":"at most 4096 bytes","kind":"fact|decision|procedure|failure_pattern|preference","event_ids":["supplied event ID"],"tags":["at most 16 tags of 128 bytes"]}]}`,
		"Extract at most 4 transferable, precisely scoped lessons from the COMPLETE history notes. Include limitations in each claim. Cite actual original event IDs. Return lessons=[] for trivial talk or no durable learning. Never turn a request, unverified assistant claim, or unfinished action into successful evidence.",
		struct {
			notes
			Coverage  HistoryCoverage   `json:"coverage"`
			Published []publishedLesson `json:"published"`
		}{notes: summary, Coverage: h.Coverage, Published: published}, generate, &extracted, func() error {
			if len(extracted.Lessons) > 4 {
				return ErrDecision
			}
			for _, l := range extracted.Lessons {
				if !boundedText(l.Claim, 4096, true) || !stringsValid(l.Tags, 16, 128) {
					return ErrDecision
				}
				switch memory.Kind(l.Kind) {
				case memory.KindFact, memory.KindDecision, memory.KindProcedure, memory.KindFailurePattern, memory.KindPreference:
				default:
					return ErrDecision
				}
				if err := citations(l.EventIDs, allowed, true); err != nil {
					return err
				}
			}
			return nil
		})
	if err != nil {
		return err
	}
	if len(extracted.Lessons) == 0 {
		return s.save(ctx, key+"/done", true)
	}
	// A new semantic routing decision can reuse a space created by Prepare,
	// including spaces created by unrelated sessions since that selection.
	r, _, err := s.route(ctx, key, "Organize these potential experience candidates. The new space, if proposed, is the destination; otherwise put the destination first in space_ids.", summary, generate)
	if err != nil {
		return err
	}
	spaces := routeSpaces(key, r)
	if len(spaces) == 0 {
		return fmt.Errorf("%w: lessons require an explicit destination", ErrDecision)
	}
	if err = s.applyRoute(ctx, key, r); err != nil {
		return err
	}
	target := spaces[0]
	if r.NewSpace != nil {
		target = spaces[len(spaces)-1]
	}
	for i, l := range extracted.Lessons {
		if _, err = s.publishLesson(ctx, fmt.Sprintf("%s/lesson/%d", key, i), target, l, h, summary, generate); err != nil {
			return err
		}
	}
	var cat catalog
	if found, err := s.load(ctx, key+"/organization-catalog", &cat); err != nil {
		return err
	} else if !found {
		// Empty text intentionally obtains all current eligible originals for
		// this small, model-selected organization scope.
		cat, err = s.retrieve(ctx, spaces, "", nil)
		if err != nil {
			return err
		}
		if err = s.save(ctx, key+"/organization-catalog", cat); err != nil {
			return err
		}
	}
	if err = s.organize(ctx, key, cat, generate); err != nil {
		return err
	}
	return s.save(ctx, key+"/done", true)
}

func (s *Service) publishLesson(ctx context.Context, key string, target model.SpaceID, l lesson, h history, summary notes, generate Generate) (*memoryItem, error) {
	publicationKey, intent, err := s.publication(ctx, key, target, l, h)
	if err != nil {
		return nil, err
	}
	if existing, err := s.existingPublication(ctx, publicationKey); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}
	operation := intent.Operation
	ids := map[string]bool{}
	for _, id := range l.EventIDs {
		ids[id] = true
	}
	records, err := s.recordsFor(ctx, h, ids)
	if err != nil {
		return nil, err
	}
	var candidate memory.Memory
	if found, err := s.load(ctx, operation+"/candidate", &candidate); err != nil {
		return nil, err
	} else if !found {
		sources, err := s.anchorRecords(ctx, records)
		if err != nil {
			return nil, err
		}
		tags := append([]string(nil), l.Tags...)
		sort.Strings(tags)
		candidate = memory.Memory{
			Space: target, Kind: memory.Kind(l.Kind), Claim: []byte(l.Claim),
			Sources: sources, Status: memory.StatusCandidate, RecordedAt: time.Now().UTC(),
			Applicability: memory.Applicability{Tags: tags},
		}
		// Strategy 1 is a model review, never a fabricated tool-validation
		// artifact. The review and rationale are retained in the journal.
		for _, source := range sources {
			candidate.Evidence = append(candidate.Evidence, memory.Evidence{Source: source, Note: "Candidate provenance. Promotion policy: " + validationMethod})
		}
		candidate.VisibleAt = candidate.RecordedAt
		candidate.ID = memory.IDFor(candidate)
		if err = s.save(ctx, operation+"/candidate", candidate); err != nil {
			return nil, err
		}
	}
	if err = s.applyMemory(ctx, operation+"/put", target, memory.Command{PutCandidate: &candidate}); err != nil {
		return nil, err
	}
	// An oversized independent review is an explicit incomplete result. The
	// candidate already exists and cannot be promoted from truncated evidence.
	input := reviewInput{Candidate: l, OriginalEvents: records, CompleteHistoryNotes: summary, Method: validationMethod}
	raw, _ := json.Marshal(input)
	if len(raw) > 256<<10 {
		return nil, fmt.Errorf("%w: independent review exceeds 256 KiB; candidate retained", ErrIncomplete)
	}
	if err = s.checkSources(ctx, Context{SessionID: h.SessionID, Sources: candidate.Sources, EvidenceReferences: evidenceReferences(records)}); err != nil {
		return nil, err
	}
	reviewEvents := make([]evidenceEvent, 0, len(records))
	for _, record := range records {
		event, excluded, projectErr := projectEvidence(record)
		if projectErr != nil {
			return nil, projectErr
		}
		if excluded == "" {
			reviewEvents = append(reviewEvents, event)
		}
	}
	var reviewed review
	err = s.decide(ctx, operation+"/review", "verify",
		`{"passed":true or false,"reason":"at most 2048 bytes","event_ids":["supporting original event ID"],"claims_tool_success":true or false,"tool_success_event_ids":["original tool.succeeded event ID"]}`,
		"Independently evaluate the candidate against the ORIGINAL events, not the extractor's confidence. Check contradictions in the full-history notes. Pass only if every material assertion, its scope and claimed outcome are supported. User and assistant text NEVER proves tool success; an assistant saying it ran a test is not a persisted tool result. Cite only original events supplied here; passing requires at least one citation. Reject unsupported generalization. This is model evidence review, never tool proof.",
		input, generate, &reviewed, func() error {
			if !boundedText(reviewed.Reason, 2048, true) {
				return ErrDecision
			}
			if err := citations(reviewed.EventIDs, ids, reviewed.Passed); err != nil {
				return err
			}
			return validateToolReview(reviewed, reviewEvents)
		})
	if err != nil {
		return nil, err
	}
	if err = s.save(ctx, operation+"/validation-method", validationMethod); err != nil {
		return nil, err
	}
	if !reviewed.Passed {
		source, sourceErr := s.memorySource(ctx, target, candidate.ID)
		if sourceErr != nil {
			return nil, sourceErr
		}
		item := &memoryItem{ID: memoryIdentity(source), Source: source, Memory: candidate}
		if err = s.save(ctx, publicationKey+"/result", item); err != nil {
			return nil, err
		}
		return item, nil
	}
	// Verify the real persisted source again before validation/promotion.
	var validation memory.Validation
	if found, err := s.load(ctx, operation+"/validation", &validation); err != nil {
		return nil, err
	} else if !found {
		validation = memory.Validation{Passed: true, StrategyVersion: 1, RecordedAt: time.Now().UTC()}
		reviewIDs := map[string]bool{}
		for _, id := range reviewed.EventIDs {
			reviewIDs[id] = true
		}
		reviewRecords, err := s.recordsFor(ctx, h, reviewIDs)
		if err != nil {
			return nil, err
		}
		validation.Evidence, err = s.anchorRecords(ctx, reviewRecords)
		if err != nil {
			return nil, err
		}
		if err = s.save(ctx, operation+"/validation", validation); err != nil {
			return nil, err
		}
	}
	if err = s.applyMemory(ctx, operation+"/validate", target, memory.Command{Validate: &memory.ValidateCommand{ID: candidate.ID, Validation: validation}}); err != nil {
		return nil, err
	}
	// Use ATE's real reducer to derive immutable successor identities rather
	// than guessing IDs from receipts or selecting the latest unrelated record.
	reducer := memory.New()
	request := model.RequestKey{Space: target, Principal: "gsea", RequestID: "derive"}
	put, err := reducer.ApplyCommand(request, memory.Command{PutCandidate: &candidate})
	if err != nil {
		return nil, err
	}
	validated, err := reducer.ApplyCommand(request, memory.Command{Validate: &memory.ValidateCommand{ID: put.Memory.ID, Validation: validation}})
	if err != nil {
		return nil, err
	}
	body, err := memory.EncodeCommand(memory.Command{Promote: &validated.Memory.ID})
	if err != nil {
		return nil, err
	}
	ack, err := s.commit(ctx, operation+"/promote", api.CommitRequest{Space: target, Method: api.MethodApplyMemory, Body: body})
	if err != nil {
		return nil, err
	}
	promoted, err := reducer.ApplyCommand(request, memory.Command{Promote: &validated.Memory.ID})
	if err != nil {
		return nil, err
	}
	source := memory.Source{SourceID: promoted.Memory.ID, Anchor: model.SnapshotAnchor{Space: target, Fence: ack.Fence}}
	item := &memoryItem{ID: memoryIdentity(source), Source: source, Memory: *promoted.Memory}
	if err = s.save(ctx, publicationKey+"/result", item); err != nil {
		return nil, err
	}
	return item, nil
}

func (s *Service) applyMemory(ctx context.Context, key string, space model.SpaceID, command memory.Command) error {
	body, err := memory.EncodeCommand(command)
	if err != nil {
		return err
	}
	_, err = s.commit(ctx, key, api.CommitRequest{Space: space, Method: api.MethodApplyMemory, Body: body})
	return err
}

func (s *Service) memorySource(ctx context.Context, space model.SpaceID, id model.ObjectID) (memory.Source, error) {
	var source memory.Source
	err := s.bridge.WithClient(ctx, func(c *client.Client) error {
		state, err := c.GetMemoryState(ctx, api.ReadMemoryStateRequest{Space: space})
		if err != nil {
			return err
		}
		for _, item := range state.Snapshot.Memories {
			if item.ID == id {
				source = memory.Source{SourceID: id, Anchor: model.SnapshotAnchor{Space: space, Fence: state.Fence}}
				return nil
			}
		}
		return ErrSelection
	})
	return source, err
}

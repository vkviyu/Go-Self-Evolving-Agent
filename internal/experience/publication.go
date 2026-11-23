package experience

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/memory"
	"github.com/vkviyu/agent-temporal-engine/model"
)

const maxSessionPublications = 256

type publicationIntent struct {
	Operation string
	Lesson    lesson
}

type publishedLesson struct {
	Publication string
	Kind        string
	Claim       string
	Destination model.SpaceID
	EventIDs    []string
	Status      string
	Source      *memory.Source
}

// publicationKey identifies a lesson by its origin and normalized claim. The
// destination Space is deliberately excluded: reorganizing a Space must not
// turn the same evidence and claim into a second publication.
func (s *Service) publicationKey(session string, l lesson, h history) (string, []EvidenceReference, error) {
	ids := map[string]bool{}
	for _, id := range l.EventIDs {
		ids[id] = true
	}
	var refs []EvidenceReference
	for _, e := range h.Events {
		if ids[e.SourceID] {
			refs = append(refs, EvidenceReference{SourceID: e.SourceID, Sequence: e.Sequence, Kind: e.Kind, OriginalSHA256: e.OriginalSHA256})
		}
	}
	if len(refs) != len(ids) || len(refs) == 0 {
		return "", nil, ErrDecision
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].SourceID < refs[j].SourceID })
	key := "experience/publication/v1/" + digest(struct {
		Namespace model.SpaceID
		Session   string
		Kind      string
		Claim     string
		Evidence  []EvidenceReference
	}{s.bridge.OperationalSpace(), session, l.Kind, strings.Join(strings.Fields(l.Claim), " "), refs})
	return key, refs, nil
}

// The insert-only index reserves the ORIGINAL operation before any mutation.
// A retry from another turn resumes that operation, including unknown outcomes.
// Normalization only folds whitespace; case can be meaningful in a fact.
func (s *Service) publication(ctx context.Context, operation string, target model.SpaceID, l lesson, h history) (string, publicationIntent, error) {
	key, _, err := s.publicationKey(h.SessionID, l, h)
	if err != nil {
		return "", publicationIntent{}, err
	}
	var intent publicationIntent
	if found, err := s.load(ctx, key+"/intent", &intent); err != nil {
		return "", intent, err
	} else if !found {
		intent = publicationIntent{Operation: operation, Lesson: l}
		if err = s.save(ctx, key+"/intent", intent); err != nil {
			return "", intent, err
		}
	}
	// Reserve a session-scoped summary slot before publication as well, so a
	// crash cannot publish a lesson invisible to later extraction comparisons.
	for i := 0; i < maxSessionPublications; i++ {
		slot := s.publicationSlot(h.SessionID, i)
		var prior publishedLesson
		found, err := s.load(ctx, slot, &prior)
		if err != nil {
			return "", intent, err
		}
		if found {
			if prior.Publication == key {
				return key, intent, nil
			}
			continue
		}
		entry := publishedLesson{Publication: key, Kind: intent.Lesson.Kind, Claim: intent.Lesson.Claim,
			Destination: target, EventIDs: intent.Lesson.EventIDs}
		return key, intent, s.save(ctx, slot, entry)
	}
	return "", intent, fmt.Errorf("%w: session publication index exceeds %d lessons", ErrIncomplete, maxSessionPublications)
}

func (s *Service) publicationSlot(session string, i int) string {
	return "experience/session-publications/v1/" + digest([]string{string(s.bridge.OperationalSpace()), session}) + fmt.Sprintf("/%d", i)
}

func (s *Service) publishedLessons(ctx context.Context, session string) ([]publishedLesson, error) {
	out := []publishedLesson{}
	for i := 0; i < maxSessionPublications; i++ {
		var entry publishedLesson
		found, err := s.load(ctx, s.publicationSlot(session, i), &entry)
		if err != nil {
			return nil, err
		}
		if !found {
			break
		}
		var result memoryItem
		if found, err = s.load(ctx, entry.Publication+"/result", &result); err != nil {
			return nil, err
		} else if found {
			entry.Status, entry.Source = string(result.Memory.Status), &result.Source
		} else {
			entry.Status = "pending_review_or_publication"
		}
		out = append(out, entry)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	if len(raw) > maxCatalogBytes {
		return nil, fmt.Errorf("%w: session publication summaries exceed %d bytes", ErrIncomplete, maxCatalogBytes)
	}
	return out, nil
}

func (s *Service) existingPublication(ctx context.Context, key string) (*memoryItem, error) {
	var item memoryItem
	found, err := s.load(ctx, key+"/result", &item)
	if err != nil || !found {
		return nil, err
	}
	// This is an exact historical publication reference, not permission to
	// adopt it. Prepare/Check still enforce current reuse eligibility.
	if err = s.bridge.WithClient(ctx, func(c *client.Client) error {
		return readMemory(ctx, c, item.Source)
	}); err != nil {
		return nil, err
	}
	return &item, nil
}

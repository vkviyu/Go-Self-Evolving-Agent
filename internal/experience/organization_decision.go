package experience

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/agent-temporal-engine/api"
)

// These DTOs deliberately omit native Memory.ID, Source.SourceID and anchors.
// Only source_id names a selectable memory; the host retains the exact mapping.
type organizationInput struct {
	Spaces    []organizationSpace    `json:"spaces"`
	Memories  []organizationMemory   `json:"memories"`
	Entries   []organizationEntry    `json:"entries"`
	Relations []organizationRelation `json:"relations"`
}

type organizationSpace struct {
	SpaceID     string `json:"space_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Archived    bool   `json:"archived"`
}

type organizationMemory struct {
	SourceID string   `json:"source_id"`
	SpaceID  string   `json:"space_id"`
	Claim    string   `json:"claim"`
	Kind     string   `json:"kind"`
	Status   string   `json:"status"`
	Tags     []string `json:"tags"`
}

type organizationEntry struct {
	TargetID    string   `json:"target_id"`
	SpaceID     string   `json:"space_id"`
	SourceID    string   `json:"source_id"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
}

type organizationRelation struct {
	TargetID    string `json:"target_id"`
	SpaceID     string `json:"space_id"`
	SourceID    string `json:"source_id"`
	ToID        string `json:"to_id"`
	Kind        string `json:"relation_kind"`
	Description string `json:"description"`
}

func organizationModelInput(cat catalog) (organizationInput, error) {
	input := organizationInput{
		Spaces: []organizationSpace{}, Memories: []organizationMemory{},
		Entries: []organizationEntry{}, Relations: []organizationRelation{},
	}
	for _, space := range cat.Spaces {
		id := string(space.ID)
		input.Spaces = append(input.Spaces, organizationSpace{id, space.Name, space.Description, space.Archived})
		for _, entry := range cat.Entries[space.ID] {
			input.Entries = append(input.Entries, organizationEntry{
				entry.ID, id, memoryIdentity(entry.Source), entry.Description, append([]string{}, entry.Tags...),
			})
		}
		for _, relation := range cat.Relations[space.ID] {
			input.Relations = append(input.Relations, organizationRelation{
				relation.ID, id, memoryIdentity(relation.From), memoryIdentity(relation.To), relation.Kind, relation.Description,
			})
		}
	}
	for _, item := range cat.Memories {
		input.Memories = append(input.Memories, organizationMemory{
			item.ID, string(item.Source.Anchor.Space), string(item.Memory.Claim),
			string(item.Memory.Kind), string(item.Memory.Status), append([]string{}, item.Memory.Applicability.Tags...),
		})
	}
	return input, budget(input, len(input.Spaces)+len(input.Memories)+len(input.Entries)+len(input.Relations))
}

const organizationInstruction = `Organize this directory with at most 16 actions. Copy action.space_id EXACTLY from spaces[].space_id.
For attach/relate, copy action.source_id EXACTLY from memories[].source_id. For relate, copy action.to_id EXACTLY from memories[].source_id as well. These strings are opaque canonical selection keys: preserve the ENTIRE string, including its space prefix and slash. A bare memory digest is invalid. Never construct, shorten, split or guess a selection key.
For editing/removing an existing membership or relation, copy target_id from entries[].target_id or relations[].target_id in the chosen space. Existing references show the same canonical keys; only memories[] supplies selectable sources. Leave target_id empty for creation. All eight action fields are required; use "" for inapplicable fields.
Rename updates name and description; archive changes metadata only. Remove affects membership, never the original memory. Attach and relate may reference cross-domain originals. Each target can be edited at most once. Preserve useful references and return actions=[] only when no change helps.
Symbolic example ONLY (the placeholders are NOT valid IDs; replace them with exact strings from the input):
{"actions":[{"kind":"attach","space_id":"<spaces[chosen].space_id>","target_id":"","source_id":"<memories[chosen].source_id>","to_id":"","name":"","description":"Why this membership helps","relation_kind":""}]}`

var errOrganizationSource = fmt.Errorf("%w: invalid supplied_source", ErrDecision)

// organizationDecision permits two invalid structured-output attempts total per
// durable decision. Invalid output is never a decision or a mutation. Provider
// errors return immediately and are not mistaken for validation feedback.
func (s *Service) organizationDecision(ctx context.Context, key string, cat catalog, generate Generate) (organization, []api.CommitRequest, error) {
	var decision organization
	decisionKey := key + "/organize"
	if found, err := s.load(ctx, decisionKey+"/decision", &decision); err != nil {
		return decision, nil, err
	} else if found {
		requests, err := organizationRequests(key, cat, decision)
		return decision, requests, err
	}
	// The input is rebuilt from the pinned catalog on every attempt, so invalid
	// output leaves no durable artifacts and a replay sees identical bytes.
	dto, err := organizationModelInput(cat)
	if err != nil {
		return decision, nil, err
	}
	input, err := json.Marshal(dto)
	if err != nil {
		return decision, nil, err
	}
	if len(input) > maxCatalogBytes {
		return decision, nil, ErrIncomplete
	}
	feedback := ""
	consumed := 0
	for consumed < 2 {
		var prior string
		found, err := s.load(ctx, fmt.Sprintf("%s/invalid-v2/%d", decisionKey, consumed), &prior)
		if err != nil {
			return decision, nil, err
		}
		if !found {
			break
		}
		feedback = prior
		consumed++
	}
	for attempt := consumed; attempt < 2; attempt++ {
		if generate == nil {
			return decision, nil, fmt.Errorf("%w: generator required for organize", ErrDecision)
		}
		instruction := organizationInstruction
		if feedback != "" {
			instruction += "\nCorrection for the previous invalid response: " + feedback +
				"\nReturn a complete replacement JSON object using the SAME original input. No proposed action has been executed."
		}
		message, err := generate(ctx, []*schema.Message{
			schema.SystemMessage(modelPolicy + "\nStage: organize\nSchema: " + organizationSchema + "\n" + instruction),
			schema.UserMessage(string(input)),
		})
		if err != nil {
			return decision, nil, err
		}
		decision = organization{}
		err = ErrDecision
		if message != nil && len(message.ToolCalls) == 0 {
			err = strictJSON([]byte(message.Content), &decision)
		}
		var requests []api.CommitRequest
		if err == nil {
			requests, err = organizationRequests(key, cat, decision)
		}
		if err == nil {
			return decision, requests, nil
		}
		// Feedback contains only host-authored text, never rejected model output
		// or arbitrary parser/engine error strings.
		feedback = "Invalid action schema or supplied target. Include every required field, obey the field limits, use only supplied IDs, and edit each target at most once."
		if errors.Is(err, errOrganizationSource) {
			feedback = "invalid supplied_source: source_id and relate.to_id must exactly equal a complete memories[].source_id. A bare digest is not a selectable source. Copy the entire canonical key without modification."
		}
		if err = s.save(ctx, fmt.Sprintf("%s/invalid-v2/%d", decisionKey, attempt), feedback); err != nil {
			return organization{}, nil, err
		}
	}
	return organization{}, nil, fmt.Errorf("%w: organize exhausted two structured-output attempts", ErrDecision)
}

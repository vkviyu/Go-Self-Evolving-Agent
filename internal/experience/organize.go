package experience

import (
	"context"
	"errors"
	"fmt"

	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/spacecatalog"
	"github.com/vkviyu/go-self-evolving-agent/internal/atebridge"
)

const organizationSchema = `{"actions":[{"kind":"rename|archive|attach|remove|relate|remove_relation","space_id":"EXACT spaces[].space_id","target_id":"EXACT entries[].target_id or relations[].target_id for edits; empty for creation","source_id":"EXACT complete memories[].source_id for attach/relate; otherwise empty","to_id":"EXACT complete memories[].source_id for relate; otherwise empty","name":"new Space name for rename; otherwise empty","description":"replacement description, at most 8192 bytes","relation_kind":"relation label for relate; otherwise empty"}]}`

// OrganizationActionResult distinguishes a rejected edit from an applied edit.
// Pending includes uncertain outcomes, which must retain their original key.
type OrganizationActionResult struct {
	Operation string
	Kind      string
	SpaceID   string
	Status    string // applied, blocked, or pending
	Code      api.ResultCode
	Conflict  api.ConflictReason
	Reason    string
}

type OrganizationReport struct {
	Complete bool
	Actions  []OrganizationActionResult
}

// MaintenanceReport reports durable organization outcomes. Complete means the
// maintenance attempt finished, not that every proposed edit was applied.
// Complete returns nil after definitive blocks; callers can surface them here.
func (s *Service) MaintenanceReport(ctx context.Context, sessionID, turnID string) (OrganizationReport, error) {
	if ctx == nil {
		return OrganizationReport{}, ErrDecision
	}
	if err := validInput(sessionID, turnID, ""); err != nil {
		return OrganizationReport{}, err
	}
	key := s.key(sessionID, turnID) + "/complete"
	report := OrganizationReport{Actions: []OrganizationActionResult{}}
	if _, err := s.load(ctx, key+"/done", &report.Complete); err != nil {
		return report, err
	}
	var decision organization
	if _, err := s.load(ctx, key+"/organize/decision", &decision); err != nil {
		return report, err
	}
	for i, a := range decision.Actions {
		operation := fmt.Sprintf("%s/organize/action/%d", key, i)
		result := OrganizationActionResult{Operation: operation, Kind: a.Kind, SpaceID: a.SpaceID, Status: "pending"}
		if _, err := s.load(ctx, operation+"/result", &result); err != nil {
			return report, err
		}
		report.Actions = append(report.Actions, result)
	}
	return report, nil
}

func definitiveOrganizationRejection(result api.CommitResult, err error) bool {
	var unknown *client.OutcomeUnknownError
	if errors.As(err, &unknown) || errors.Is(err, atebridge.ErrOutcomeUnknown) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return result.Code == api.CodeRejected ||
		result.Code == api.CodeConflict && result.Conflict == api.ConflictPrecondition
}

func (s *Service) organize(ctx context.Context, key string, cat catalog, generate Generate) error {
	// A pinned catalog is the exact host-side mapping and CAS versions shared by
	// corrective retries and direct organize replays. Pin it only once a valid
	// decision exists, so an invalid model response leaves no durable mutation.
	pinned := false
	if found, err := s.load(ctx, key+"/organization-catalog", &cat); err != nil {
		return err
	} else {
		pinned = found
	}
	decision, requests, err := s.organizationDecision(ctx, key, cat, generate)
	if err != nil {
		return err
	}
	if !pinned {
		if err = s.save(ctx, key+"/organization-catalog", cat); err != nil {
			return err
		}
	}
	if err = s.save(ctx, key+"/organize/decision", decision); err != nil {
		return err
	}
	// Decisions already contain the exact catalog version used for every CAS.
	// Definitive rejections are terminal for this decision. Unknown outcomes
	// remain pending under the original key; no rebasing or new-key retry.
	for i, request := range requests {
		operation := fmt.Sprintf("%s/organize/action/%d", key, i)
		var result OrganizationActionResult
		if found, err := s.load(ctx, operation+"/result", &result); err != nil {
			return err
		} else if found {
			if result.Status != "applied" && result.Status != "blocked" {
				return ErrDecision
			}
			continue
		}
		ack, commitErr := s.commit(ctx, operation, request)
		result = OrganizationActionResult{Operation: operation, Kind: decision.Actions[i].Kind,
			SpaceID: string(request.Space), Status: "applied", Code: ack.Code, Conflict: ack.Conflict}
		if commitErr != nil {
			if !definitiveOrganizationRejection(ack, commitErr) {
				return commitErr
			}
			result.Status = "blocked"
			result.Reason = fmt.Sprintf("ATE rejected the original %s action (%s, %s); it was not applied. A later turn may propose a fresh action against the current catalog.", result.Kind, ack.Code, ack.Conflict)
		}
		if err = s.save(ctx, operation+"/result", result); err != nil {
			return err
		}
	}
	return nil
}

func organizationRequests(key string, cat catalog, decision organization) ([]api.CommitRequest, error) {
	if len(decision.Actions) > 16 {
		return nil, ErrDecision
	}
	spaces := map[string]spacecatalog.Space{}
	for _, v := range cat.Spaces {
		spaces[string(v.ID)] = v
	}
	memories := map[string]memoryItem{}
	for _, v := range cat.Memories {
		memories[v.ID] = v
	}
	seen := map[string]bool{}
	var out []api.CommitRequest
	for i, a := range decision.Actions {
		space, ok := spaces[a.SpaceID]
		if !ok || operational(space.ID) {
			return nil, ErrDecision
		}
		if !boundedText(a.Description, 8192, false) || !boundedText(a.Name, 512, false) || !boundedText(a.RelationKind, 128, false) {
			return nil, ErrDecision
		}
		request := api.CommitRequest{Space: space.ID}
		var err error
		target := a.SpaceID + "/"
		switch a.Kind {
		case "rename", "archive":
			if a.TargetID != "" || a.SourceID != "" || a.ToID != "" || a.RelationKind != "" {
				return nil, ErrDecision
			}
			command := spacecatalog.Apply{ExpectedVersion: space.Version, Name: space.Name, Description: space.Description, Archived: space.Archived}
			if a.Kind == "rename" {
				if !boundedText(a.Name, 512, true) {
					return nil, ErrDecision
				}
				command.Name, command.Description = a.Name, a.Description
			} else {
				if a.Name != "" || a.Description != "" {
					return nil, ErrDecision
				}
				command.Archived = true
			}
			request.Method = api.MethodApplySpace
			request.Body, err = spacecatalog.EncodeApply(command)
			target += "metadata"
		case "attach":
			if a.Name != "" || a.ToID != "" || a.RelationKind != "" {
				return nil, ErrDecision
			}
			item, ok := memories[a.SourceID]
			if !ok {
				return nil, errOrganizationSource
			}
			edit := spacecatalog.EntryEdit{Source: item.Source, Description: a.Description}
			if a.TargetID != "" {
				found := false
				for _, entry := range cat.Entries[space.ID] {
					if entry.ID == a.TargetID && spacecatalog.SameMemory(entry.Source, item.Source) {
						edit.ID, edit.ExpectedVersion, edit.Source, edit.Tags = entry.ID, entry.Version, entry.Source, entry.Tags
						found = true
						break
					}
				}
				if !found {
					return nil, ErrDecision
				}
			} else {
				for _, entry := range cat.Entries[space.ID] {
					if spacecatalog.SameMemory(entry.Source, item.Source) {
						return nil, ErrDecision
					}
				}
				edit.ID = "gsea-entry-" + digest([]string{key, fmt.Sprint(i)})
			}
			request.Method = api.MethodOrganizeMemory
			request.Body, err = spacecatalog.EncodeOrganize(spacecatalog.Organize{Entry: &edit})
			target += "entry/" + edit.ID
		case "remove":
			if a.SourceID != "" || a.ToID != "" || a.Name != "" || a.Description != "" || a.RelationKind != "" {
				return nil, ErrDecision
			}
			var edit *spacecatalog.EntryEdit
			for _, entry := range cat.Entries[space.ID] {
				if entry.ID == a.TargetID {
					edit = &spacecatalog.EntryEdit{ID: entry.ID, ExpectedVersion: entry.Version, Source: entry.Source, Description: entry.Description, Tags: entry.Tags, Removed: true}
					break
				}
			}
			if edit == nil {
				return nil, ErrDecision
			}
			request.Method = api.MethodOrganizeMemory
			request.Body, err = spacecatalog.EncodeOrganize(spacecatalog.Organize{Entry: edit})
			target += "entry/" + edit.ID
		case "relate":
			if a.Name != "" || !boundedText(a.RelationKind, 128, true) {
				return nil, ErrDecision
			}
			from, ok := memories[a.SourceID]
			if !ok {
				return nil, errOrganizationSource
			}
			to, ok := memories[a.ToID]
			if !ok {
				return nil, errOrganizationSource
			}
			edit := spacecatalog.RelationEdit{From: from.Source, To: to.Source, Kind: a.RelationKind, Description: a.Description}
			if a.TargetID != "" {
				found := false
				for _, relation := range cat.Relations[space.ID] {
					if relation.ID == a.TargetID && spacecatalog.SameMemory(relation.From, edit.From) && spacecatalog.SameMemory(relation.To, edit.To) {
						edit.ID, edit.ExpectedVersion, edit.From, edit.To = relation.ID, relation.Version, relation.From, relation.To
						found = true
						break
					}
				}
				if !found {
					return nil, ErrDecision
				}
			} else {
				edit.ID = "gsea-relation-" + digest([]string{key, fmt.Sprint(i)})
			}
			request.Method = api.MethodOrganizeMemory
			request.Body, err = spacecatalog.EncodeOrganize(spacecatalog.Organize{Relation: &edit})
			target += "relation/" + edit.ID
		case "remove_relation":
			if a.SourceID != "" || a.ToID != "" || a.Name != "" || a.Description != "" || a.RelationKind != "" {
				return nil, ErrDecision
			}
			var edit *spacecatalog.RelationEdit
			for _, r := range cat.Relations[space.ID] {
				if r.ID == a.TargetID {
					edit = &spacecatalog.RelationEdit{ID: r.ID, ExpectedVersion: r.Version, From: r.From, To: r.To, Kind: r.Kind, Description: r.Description, Removed: true}
					break
				}
			}
			if edit == nil {
				return nil, ErrDecision
			}
			request.Method = api.MethodOrganizeMemory
			request.Body, err = spacecatalog.EncodeOrganize(spacecatalog.Organize{Relation: edit})
			target += "relation/" + edit.ID
		default:
			return nil, ErrDecision
		}
		if err != nil {
			return nil, err
		}
		if seen[target] {
			return nil, ErrDecision
		}
		seen[target] = true
		out = append(out, request)
	}
	return out, nil
}

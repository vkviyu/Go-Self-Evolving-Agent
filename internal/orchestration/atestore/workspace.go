package atestore

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

const workspaceViewKind = "workspace-view"

// Idempotency receipts are individual authority records, scoped by operation
// and team. Correlation IDs are deliberately excluded from request identity.
type workspaceIdentity struct {
	ID   string `json:"id"`
	Hash string `json:"hash"`
}

// workspaceContext supplies a background context for workspace repository calls whose
// caller omitted one.
func workspaceContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// workspaceDigest hashes the JSON representation of the workspace operation for identity
// checks.
func workspaceDigest(value any) string {
	encoded, _ := json.Marshal(value)
	return fmt.Sprintf("%x", sha256.Sum256(encoded))
}

func workspaceIdentityID(team orchestration.TeamID, key string) string {
	return workspaceDigest([2]string{string(team), key})
}

func workspaceViewHash(spec workspacecoord.ViewSpec) string {
	return workspaceDigest(struct {
		TeamID        orchestration.TeamID    `json:"team_id"`
		AgentID       orchestration.AgentID   `json:"agent_id"`
		TaskID        orchestration.TaskID    `json:"task_id"`
		Mode          workspacecoord.ViewMode `json:"mode"`
		WorkspaceRoot string                  `json:"workspace_root"`
		RootPath      string                  `json:"root_path"`
		BaseRevision  string                  `json:"base_revision"`
	}{spec.TeamID, spec.AgentID, spec.TaskID, spec.Mode, spec.WorkspaceRoot, spec.RootPath, spec.BaseRevision})
}

func workspaceAgentTeam(tx *ateauthority.Tx, id orchestration.AgentID, team orchestration.TeamID) error {
	agent, err := getRecord[orchestration.AgentSnapshot](tx, "agent", string(id))
	if err != nil {
		return err
	}
	if agent.TeamID != team {
		return fmt.Errorf("%w: Agent %s belongs to another team", orchestration.ErrInvalidEvent, id)
	}
	return nil
}

func workspaceTaskTeam(tx *ateauthority.Tx, id orchestration.TaskID, team orchestration.TeamID) error {
	if id == "" {
		return nil
	}
	task, err := getRecord[orchestration.TaskSnapshot](tx, "task", string(id))
	if err != nil {
		return err
	}
	if task.TeamID != team {
		return fmt.Errorf("%w: task %s belongs to another team", orchestration.ErrInvalidEvent, id)
	}
	return nil
}

// workspaceAppendEvents stages workspace events in the owning team's stream in the
// caller's transaction.
func workspaceAppendEvents(tx *ateauthority.Tx, team orchestration.TeamID, events ...orchestration.PendingEvent) error {
	_, err := appendEvents(tx, orchestration.Stream{Type: orchestration.StreamWorkspace, ID: string(team), TeamID: team}, orchestration.AnySequence, events...)
	return err
}

// CreateView persists the view specification and initial lifecycle state.
func (store *Store) CreateView(ctx context.Context, spec workspacecoord.ViewSpec) (workspacecoord.View, error) {
	// Validation inspects paths; keep it outside the retriable callback.
	if err := spec.Validate(); err != nil {
		return workspacecoord.View{}, err
	}
	id, err := orchestration.NewWorkspaceViewID()
	if err != nil {
		return workspacecoord.View{}, err
	}
	now, hash := time.Now().UTC(), workspaceViewHash(spec)
	var result workspacecoord.View
	err = store.authority.Update(workspaceContext(ctx), func(tx *ateauthority.Tx) error {
		if spec.IdempotencyKey != "" {
			identity, err := getRecord[workspaceIdentity](tx, "workspace-view-key", workspaceIdentityID(spec.TeamID, spec.IdempotencyKey))
			if err == nil {
				view, err := getRecord[workspacecoord.View](tx, workspaceViewKind, identity.ID)
				if err != nil {
					return err
				}
				if identity.Hash != "" && identity.Hash != hash || identity.Hash == "" && !workspaceViewMatches(view, spec) {
					return fmt.Errorf("%w: workspace view key %q", orchestration.ErrIdempotencyConflict, spec.IdempotencyKey)
				}
				result = view
				return nil
			}
			if !errors.Is(err, orchestration.ErrNotFound) {
				return err
			}
		}
		if err := workspaceAgentTeam(tx, spec.AgentID, spec.TeamID); err != nil {
			return err
		}
		if err := workspaceTaskTeam(tx, spec.TaskID, spec.TeamID); err != nil {
			return err
		}
		view := workspacecoord.View{
			ID: id, TeamID: spec.TeamID, AgentID: spec.AgentID, TaskID: spec.TaskID,
			Mode: spec.Mode, WorkspaceRoot: spec.WorkspaceRoot, RootPath: spec.RootPath,
			BaseRevision: spec.BaseRevision, State: workspacecoord.ViewActive,
			CorrelationID: spec.CorrelationID, IdempotencyKey: spec.IdempotencyKey,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := createRecord(tx, workspaceViewKind, string(id), view); err != nil {
			return err
		}
		if spec.IdempotencyKey != "" {
			if err := createRecord(tx, "workspace-view-key", workspaceIdentityID(spec.TeamID, spec.IdempotencyKey), workspaceIdentity{ID: string(id), Hash: hash}); err != nil {
				return err
			}
		}
		if err := workspaceAppendEvents(tx, spec.TeamID, orchestration.PendingEvent{
			Type: orchestration.EventWorkspaceViewCreated, ActorAgentID: spec.AgentID,
			CorrelationID: spec.CorrelationID, IdempotencyKey: spec.IdempotencyKey,
			Payload: orchestration.MustJSONPayload(map[string]any{"view_id": id, "agent_id": spec.AgentID, "mode": spec.Mode, "root_path": spec.RootPath, "base_revision": spec.BaseRevision}),
		}); err != nil {
			return err
		}
		result = view
		return nil
	})
	if err != nil {
		return workspacecoord.View{}, err
	}
	return result, nil
}

func workspaceViewMatches(view workspacecoord.View, spec workspacecoord.ViewSpec) bool {
	return view.TeamID == spec.TeamID && view.AgentID == spec.AgentID && view.TaskID == spec.TaskID &&
		view.Mode == spec.Mode && view.WorkspaceRoot == spec.WorkspaceRoot &&
		view.RootPath == spec.RootPath && view.BaseRevision == spec.BaseRevision
}

// GetView loads the workspace view metadata by ID.
func (store *Store) GetView(ctx context.Context, id orchestration.WorkspaceViewID) (workspacecoord.View, error) {
	if err := id.Validate(); err != nil {
		return workspacecoord.View{}, err
	}
	var view workspacecoord.View
	err := store.authority.View(workspaceContext(ctx), func(tx *ateauthority.Tx) error {
		var err error
		view, err = getRecord[workspacecoord.View](tx, workspaceViewKind, string(id))
		return err
	})
	return view, err
}

// ListViews returns the stored workspace views matching the query.
func (store *Store) ListViews(ctx context.Context, query workspacecoord.ViewQuery) ([]workspacecoord.View, error) {
	if err := query.TeamID.Validate(); err != nil {
		return nil, err
	}
	if query.AgentID != "" {
		if err := query.AgentID.Validate(); err != nil {
			return nil, err
		}
	}
	var result []workspacecoord.View
	err := store.authority.View(workspaceContext(ctx), func(tx *ateauthority.Tx) error {
		views, err := listRecords[workspacecoord.View](tx, workspaceViewKind)
		if err != nil {
			return err
		}
		result = make([]workspacecoord.View, 0)
		for _, view := range views {
			if view.TeamID == query.TeamID && (query.AgentID == "" || view.AgentID == query.AgentID) &&
				(len(query.States) == 0 || slices.Contains(query.States, view.State)) {
				result = append(result, view)
			}
		}
		slices.SortFunc(result, func(a, b workspacecoord.View) int {
			if order := a.CreatedAt.Compare(b.CreatedAt); order != 0 {
				return order
			}
			return slices.Compare([]byte(a.ID), []byte(b.ID))
		})
		return nil
	})
	return result, err
}

// ReleaseView records the release of a view and the reason for its state transition.
func (store *Store) ReleaseView(ctx context.Context, id orchestration.WorkspaceViewID, reason string) (workspacecoord.View, error) {
	if err := id.Validate(); err != nil {
		return workspacecoord.View{}, err
	}
	now := time.Now().UTC()
	var result workspacecoord.View
	err := store.authority.Update(workspaceContext(ctx), func(tx *ateauthority.Tx) error {
		var err error
		result, err = workspaceReleaseViewTx(tx, id, reason, now)
		return err
	})
	if err != nil {
		return workspacecoord.View{}, err
	}
	return result, nil
}

func workspaceReleaseViewTx(tx *ateauthority.Tx, id orchestration.WorkspaceViewID, reason string, now time.Time) (workspacecoord.View, error) {
	view, err := getRecord[workspacecoord.View](tx, workspaceViewKind, string(id))
	if err != nil || view.State != workspacecoord.ViewActive {
		return view, err
	}
	view.State, view.UpdatedAt, view.ReleasedAt = workspacecoord.ViewReleased, now, &now
	if err := putRecord(tx, workspaceViewKind, string(id), view); err != nil {
		return workspacecoord.View{}, err
	}
	err = workspaceAppendEvents(tx, view.TeamID, orchestration.PendingEvent{
		Type: orchestration.EventWorkspaceViewReleased, ActorAgentID: view.AgentID, CorrelationID: view.CorrelationID,
		Payload: orchestration.MustJSONPayload(map[string]any{"view_id": id, "reason": reason}),
	})
	return view, err
}

var _ workspacecoord.Repository = (*Store)(nil)
var _ workspacecoord.ResolutionLinker = (*Store)(nil)

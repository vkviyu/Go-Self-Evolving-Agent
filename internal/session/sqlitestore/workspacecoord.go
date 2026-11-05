package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

func hashViewSpec(spec workspacecoord.ViewSpec) string {
	request := struct {
		TeamID        orchestration.TeamID    `json:"team_id"`
		AgentID       orchestration.AgentID   `json:"agent_id"`
		TaskID        orchestration.TaskID    `json:"task_id"`
		Mode          workspacecoord.ViewMode `json:"mode"`
		WorkspaceRoot string                  `json:"workspace_root"`
		RootPath      string                  `json:"root_path"`
		BaseRevision  string                  `json:"base_revision"`
	}{spec.TeamID, spec.AgentID, spec.TaskID, spec.Mode, spec.WorkspaceRoot, spec.RootPath, spec.BaseRevision}
	encoded, _ := json.Marshal(request)
	return sha256Hex(encoded)
}

func hashMergeSpec(spec workspacecoord.MergeSpec) string {
	request := struct {
		TeamID        orchestration.TeamID          `json:"team_id"`
		AgentID       orchestration.AgentID         `json:"agent_id"`
		TaskID        orchestration.TaskID          `json:"task_id"`
		ViewID        orchestration.WorkspaceViewID `json:"view_id"`
		BaseRevision  string                        `json:"base_revision"`
		PatchArtifact string                        `json:"patch_artifact"`
	}{spec.TeamID, spec.AgentID, spec.TaskID, spec.ViewID, spec.BaseRevision, spec.PatchArtifact}
	encoded, _ := json.Marshal(request)
	// Delivery replacement is part of the operation for both merge kinds.
	// Plain git-patch requests retain the original identity for compatibility.
	if spec.Kind == workspacecoord.MergeSnapshot || spec.Supersedes != "" {
		encoded, _ = json.Marshal(struct {
			Base       json.RawMessage          `json:"base"`
			Kind       workspacecoord.MergeKind `json:"kind"`
			Candidate  string                   `json:"candidate_digest"`
			Supersedes orchestration.MergeID    `json:"supersedes"`
		}{encoded, spec.Kind, spec.CandidateDigest, spec.Supersedes})
	}
	return sha256Hex(encoded)
}

func (store *OrchestrationStore) CreateView(ctx context.Context, spec workspacecoord.ViewSpec) (workspacecoord.View, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := spec.Validate(); err != nil {
		return workspacecoord.View{}, err
	}
	now := time.Now().UTC()
	hash := hashViewSpec(spec)
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return workspacecoord.View{}, fmt.Errorf("begin workspace view: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if spec.IdempotencyKey != "" {
		var viewID, storedHash string
		lookupErr := tx.QueryRowContext(ctx, `SELECT view_id, request_hash FROM workspace_views WHERE team_id = ? AND idempotency_key = ?`, string(spec.TeamID), spec.IdempotencyKey).Scan(&viewID, &storedHash)
		if lookupErr == nil {
			if storedHash != "" && storedHash != hash {
				return workspacecoord.View{}, fmt.Errorf("%w: workspace view key %q", orchestration.ErrIdempotencyConflict, spec.IdempotencyKey)
			}
			view, getErr := viewByID(ctx, tx, orchestration.WorkspaceViewID(viewID))
			if getErr != nil {
				return workspacecoord.View{}, getErr
			}
			if storedHash == "" && !legacyViewMatches(view, spec) {
				return workspacecoord.View{}, fmt.Errorf("%w: legacy workspace view key %q", orchestration.ErrIdempotencyConflict, spec.IdempotencyKey)
			}
			if err := tx.Commit(); err != nil {
				return workspacecoord.View{}, err
			}
			return view, nil
		}
		if !errors.Is(lookupErr, sql.ErrNoRows) {
			return workspacecoord.View{}, fmt.Errorf("inspect workspace view idempotency: %w", lookupErr)
		}
	}
	if err := agentBelongsToTeam(ctx, tx, spec.AgentID, spec.TeamID); err != nil {
		return workspacecoord.View{}, err
	}
	viewID, err := orchestration.NewWorkspaceViewID()
	if err != nil {
		return workspacecoord.View{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO workspace_views(view_id, team_id, agent_id, task_id, mode, workspace_root, root_path, base_revision, state, correlation_id, idempotency_key, request_hash, created_at, updated_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, string(viewID), string(spec.TeamID), string(spec.AgentID), string(spec.TaskID), string(spec.Mode), spec.WorkspaceRoot, spec.RootPath, spec.BaseRevision, string(workspacecoord.ViewActive), spec.CorrelationID, spec.IdempotencyKey, hash, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		return workspacecoord.View{}, fmt.Errorf("insert workspace view: %w", err)
	}
	sequence, err := currentOrchestrationSequence(ctx, tx, orchestration.StreamWorkspace, string(spec.TeamID))
	if err != nil {
		return workspacecoord.View{}, err
	}
	if _, err := appendOrchestrationEvents(ctx, tx, orchestration.Stream{Type: orchestration.StreamWorkspace, ID: string(spec.TeamID), TeamID: spec.TeamID}, sequence, orchestration.PendingEvent{Type: orchestration.EventWorkspaceViewCreated, ActorAgentID: spec.AgentID, CorrelationID: spec.CorrelationID, IdempotencyKey: spec.IdempotencyKey, Payload: orchestration.MustJSONPayload(map[string]any{"view_id": viewID, "agent_id": spec.AgentID, "mode": spec.Mode, "root_path": spec.RootPath, "base_revision": spec.BaseRevision})}); err != nil {
		return workspacecoord.View{}, err
	}
	view, err := viewByID(ctx, tx, viewID)
	if err != nil {
		return workspacecoord.View{}, err
	}
	if err := tx.Commit(); err != nil {
		return workspacecoord.View{}, fmt.Errorf("commit workspace view: %w", err)
	}
	return view, nil
}

func (store *OrchestrationStore) GetView(ctx context.Context, viewID orchestration.WorkspaceViewID) (workspacecoord.View, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := viewID.Validate(); err != nil {
		return workspacecoord.View{}, err
	}
	view, err := viewByID(ctx, store.db, viewID)
	if errors.Is(err, sql.ErrNoRows) {
		return workspacecoord.View{}, fmt.Errorf("%w: workspace view %s", orchestration.ErrNotFound, viewID)
	}
	return view, err
}

func (store *OrchestrationStore) ListViews(ctx context.Context, query workspacecoord.ViewQuery) ([]workspacecoord.View, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := query.TeamID.Validate(); err != nil {
		return nil, err
	}
	conditions := []string{"team_id = ?"}
	args := []any{string(query.TeamID)}
	if query.AgentID != "" {
		if err := query.AgentID.Validate(); err != nil {
			return nil, err
		}
		conditions = append(conditions, "agent_id = ?")
		args = append(args, string(query.AgentID))
	}
	if len(query.States) > 0 {
		placeholders := make([]string, len(query.States))
		for index, state := range query.States {
			placeholders[index] = "?"
			args = append(args, string(state))
		}
		conditions = append(conditions, "state IN ("+strings.Join(placeholders, ",")+")")
	}
	rows, err := store.db.QueryContext(ctx, `SELECT view_id, team_id, agent_id, task_id, mode, workspace_root, root_path, base_revision, state, correlation_id, idempotency_key, created_at, updated_at, released_at FROM workspace_views WHERE `+strings.Join(conditions, " AND ")+` ORDER BY created_at ASC, view_id ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	views := make([]workspacecoord.View, 0)
	for rows.Next() {
		view, scanErr := scanView(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		views = append(views, view)
	}
	return views, rows.Err()
}

func (store *OrchestrationStore) ReleaseView(ctx context.Context, viewID orchestration.WorkspaceViewID, reason string) (workspacecoord.View, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := viewID.Validate(); err != nil {
		return workspacecoord.View{}, err
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return workspacecoord.View{}, err
	}
	defer func() { _ = tx.Rollback() }()
	view, err := viewByID(ctx, tx, viewID)
	if errors.Is(err, sql.ErrNoRows) {
		return workspacecoord.View{}, fmt.Errorf("%w: workspace view %s", orchestration.ErrNotFound, viewID)
	}
	if err != nil {
		return workspacecoord.View{}, err
	}
	if view.State != workspacecoord.ViewActive {
		if err := tx.Commit(); err != nil {
			return workspacecoord.View{}, err
		}
		return view, nil
	}
	now := time.Now().UTC()
	if err := execCAS(ctx, tx, orchestration.ErrSequenceConflict, `UPDATE workspace_views SET state = ?, updated_at = ?, released_at = ? WHERE view_id = ? AND state = ?`, string(workspacecoord.ViewReleased), now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), string(viewID), string(workspacecoord.ViewActive)); err != nil {
		return workspacecoord.View{}, err
	}
	sequence, err := currentOrchestrationSequence(ctx, tx, orchestration.StreamWorkspace, string(view.TeamID))
	if err != nil {
		return workspacecoord.View{}, err
	}
	if _, err := appendOrchestrationEvents(ctx, tx, orchestration.Stream{Type: orchestration.StreamWorkspace, ID: string(view.TeamID), TeamID: view.TeamID}, sequence, orchestration.PendingEvent{Type: orchestration.EventWorkspaceViewReleased, ActorAgentID: view.AgentID, CorrelationID: view.CorrelationID, Payload: orchestration.MustJSONPayload(map[string]any{"view_id": viewID, "reason": reason})}); err != nil {
		return workspacecoord.View{}, err
	}
	updated, err := viewByID(ctx, tx, viewID)
	if err != nil {
		return workspacecoord.View{}, err
	}
	if err := tx.Commit(); err != nil {
		return workspacecoord.View{}, err
	}
	return updated, nil
}

func (store *OrchestrationStore) EnqueueMerge(ctx context.Context, spec workspacecoord.MergeSpec) (workspacecoord.MergeRequest, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := spec.Validate(); err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	hash := hashMergeSpec(spec)
	now := time.Now().UTC()
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if spec.IdempotencyKey != "" {
		var mergeID, storedHash string
		lookupErr := tx.QueryRowContext(ctx, `SELECT merge_id, request_hash FROM merge_queue WHERE team_id = ? AND idempotency_key = ?`, string(spec.TeamID), spec.IdempotencyKey).Scan(&mergeID, &storedHash)
		if lookupErr == nil {
			merge, getErr := mergeByID(ctx, tx, orchestration.MergeID(mergeID))
			if getErr != nil {
				return workspacecoord.MergeRequest{}, getErr
			}
			legacy := spec
			legacy.Supersedes = ""
			// Older git-patch hashes did not cover Supersedes. Only accept that
			// format after comparing the exact stored replacement relationship.
			oldGitHash := spec.Kind != workspacecoord.MergeSnapshot && storedHash == hashMergeSpec(legacy)
			legacyHash := storedHash == "" || oldGitHash
			if (!legacyHash && storedHash != hash) || (legacyHash && !legacyMergeMatches(merge, spec)) {
				return workspacecoord.MergeRequest{}, fmt.Errorf("%w: legacy merge key %q", orchestration.ErrIdempotencyConflict, spec.IdempotencyKey)
			}
			if err := tx.Commit(); err != nil {
				return workspacecoord.MergeRequest{}, err
			}
			return merge, nil
		}
		if !errors.Is(lookupErr, sql.ErrNoRows) {
			return workspacecoord.MergeRequest{}, lookupErr
		}
	}
	view, err := viewByID(ctx, tx, spec.ViewID)
	if err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	if view.TeamID != spec.TeamID || view.AgentID != spec.AgentID {
		return workspacecoord.MergeRequest{}, fmt.Errorf("%w: merge view owner mismatch", orchestration.ErrInvalidEvent)
	}
	if view.State != workspacecoord.ViewActive {
		return workspacecoord.MergeRequest{}, fmt.Errorf("%w: merge view is %s", orchestration.ErrInvalidEvent, view.State)
	}
	if view.BaseRevision != "" && view.BaseRevision != spec.BaseRevision {
		return workspacecoord.MergeRequest{}, fmt.Errorf("%w: merge base revision does not match workspace view", orchestration.ErrInvalidEvent)
	}
	mergeID, err := orchestration.NewMergeID()
	if err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	kind := spec.Kind
	if kind == "" {
		kind = workspacecoord.MergeGitPatch
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO merge_queue(merge_id, team_id, agent_id, task_id, view_id, base_revision, patch_artifact, state, reason, correlation_id, idempotency_key, request_hash, version, created_at, updated_at, kind, candidate_digest, supersedes) VALUES(?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?, 1, ?, ?, ?, ?, ?)`, string(mergeID), string(spec.TeamID), string(spec.AgentID), string(spec.TaskID), string(spec.ViewID), spec.BaseRevision, spec.PatchArtifact, string(workspacecoord.MergePending), spec.CorrelationID, spec.IdempotencyKey, hash, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), string(kind), spec.CandidateDigest, string(spec.Supersedes)); err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	sequence, err := currentOrchestrationSequence(ctx, tx, orchestration.StreamWorkspace, string(spec.TeamID))
	if err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	stream := orchestration.Stream{Type: orchestration.StreamWorkspace, ID: string(spec.TeamID), TeamID: spec.TeamID}
	queued := orchestration.PendingEvent{Type: orchestration.EventMergeQueued, ActorAgentID: spec.AgentID, CorrelationID: spec.CorrelationID, IdempotencyKey: spec.IdempotencyKey, Payload: orchestration.MustJSONPayload(map[string]any{"merge_id": mergeID, "view_id": spec.ViewID, "base_revision": spec.BaseRevision, "patch_artifact": spec.PatchArtifact, "kind": kind, "candidate_digest": spec.CandidateDigest, "supersedes": spec.Supersedes})}
	events := []orchestration.PendingEvent{queued}
	if spec.Supersedes != "" {
		superseded, supersedeErr := supersedeMergeTx(ctx, tx, spec, mergeID, now)
		if supersedeErr != nil {
			return workspacecoord.MergeRequest{}, supersedeErr
		}
		events = append(events, superseded)
	}
	if _, err := appendOrchestrationEvents(ctx, tx, stream, sequence, events...); err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	merge, err := mergeByID(ctx, tx, mergeID)
	if err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	if err := tx.Commit(); err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	return merge, nil
}

// supersedeMergeTx marks the open merge named by spec.Supersedes as replaced
// by mergeID and returns the event to record.
func supersedeMergeTx(ctx context.Context, tx *sql.Tx, spec workspacecoord.MergeSpec, mergeID orchestration.MergeID, now time.Time) (orchestration.PendingEvent, error) {
	previous, err := mergeByID(ctx, tx, spec.Supersedes)
	if errors.Is(err, sql.ErrNoRows) {
		return orchestration.PendingEvent{}, fmt.Errorf("%w: superseded merge %s", orchestration.ErrNotFound, spec.Supersedes)
	}
	if err != nil {
		return orchestration.PendingEvent{}, err
	}
	if previous.TeamID != spec.TeamID {
		return orchestration.PendingEvent{}, fmt.Errorf("%w: superseded merge belongs to another Team", orchestration.ErrInvalidEvent)
	}
	sameTask := spec.TaskID != "" && previous.TaskID == spec.TaskID
	resolution := spec.TaskID != "" && previous.ResolutionTaskID == spec.TaskID && previous.State == workspacecoord.MergeConflicted
	sameUntaskedView := spec.TaskID == "" && previous.TaskID == "" && previous.ViewID == spec.ViewID && previous.AgentID == spec.AgentID
	if !sameTask && !resolution && !sameUntaskedView {
		return orchestration.PendingEvent{}, fmt.Errorf("%w: replacement is not a revision or resolution of the previous delivery", orchestration.ErrInvalidEvent)
	}
	if err := validateMergeTransition(previous.State, workspacecoord.MergeSuperseded); err != nil {
		return orchestration.PendingEvent{}, err
	}
	if err := execCAS(ctx, tx, orchestration.ErrSequenceConflict, `UPDATE merge_queue SET state = ?, superseded_by = ?, reason = ?, version = version + 1, updated_at = ? WHERE merge_id = ? AND version = ?`, string(workspacecoord.MergeSuperseded), string(mergeID), "superseded by "+string(mergeID), now.Format(time.RFC3339Nano), string(previous.ID), previous.Version); err != nil {
		return orchestration.PendingEvent{}, err
	}
	return orchestration.PendingEvent{Type: orchestration.EventMergeSuperseded, ActorAgentID: spec.AgentID, CorrelationID: spec.CorrelationID, Payload: orchestration.MustJSONPayload(map[string]any{"merge_id": previous.ID, "from": previous.State, "superseded_by": mergeID})}, nil
}

func (store *OrchestrationStore) GetMerge(ctx context.Context, mergeID orchestration.MergeID) (workspacecoord.MergeRequest, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := mergeID.Validate(); err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	merge, err := mergeByID(ctx, store.db, mergeID)
	if errors.Is(err, sql.ErrNoRows) {
		return workspacecoord.MergeRequest{}, fmt.Errorf("%w: merge %s", orchestration.ErrNotFound, mergeID)
	}
	return merge, err
}

func (store *OrchestrationStore) ListMerges(ctx context.Context, query workspacecoord.MergeQuery) ([]workspacecoord.MergeRequest, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := query.TeamID.Validate(); err != nil {
		return nil, err
	}
	conditions := []string{"team_id = ?"}
	args := []any{string(query.TeamID)}
	if len(query.States) > 0 {
		placeholders := make([]string, len(query.States))
		for index, state := range query.States {
			placeholders[index] = "?"
			args = append(args, string(state))
		}
		conditions = append(conditions, "state IN ("+strings.Join(placeholders, ",")+")")
	}
	for _, filter := range []struct {
		column string
		value  string
	}{{"view_id", string(query.ViewID)}, {"task_id", string(query.TaskID)}, {"resolution_task_id", string(query.ResolutionTaskID)}} {
		if filter.value != "" {
			conditions = append(conditions, filter.column+" = ?")
			args = append(args, filter.value)
		}
	}
	rows, err := store.db.QueryContext(ctx, `SELECT `+mergeColumns+` FROM merge_queue WHERE `+strings.Join(conditions, " AND ")+` ORDER BY created_at ASC, merge_id ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	merges := make([]workspacecoord.MergeRequest, 0)
	for rows.Next() {
		merge, scanErr := scanMerge(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		merges = append(merges, merge)
	}
	return merges, rows.Err()
}

func (store *OrchestrationStore) TransitionMerge(ctx context.Context, transition workspacecoord.MergeTransition) (workspacecoord.MergeRequest, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := transition.MergeID.Validate(); err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	if transition.ActorAgentID != "" {
		if err := transition.ActorAgentID.Validate(); err != nil {
			return workspacecoord.MergeRequest{}, err
		}
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	defer func() { _ = tx.Rollback() }()
	merge, err := mergeByID(ctx, tx, transition.MergeID)
	if errors.Is(err, sql.ErrNoRows) {
		return workspacecoord.MergeRequest{}, fmt.Errorf("%w: merge %s", orchestration.ErrNotFound, transition.MergeID)
	}
	if err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	if merge.State == workspacecoord.MergeConflicted && transition.Target == workspacecoord.MergePending {
		if transition.ResolutionTaskID == "" || merge.ResolutionTaskID != transition.ResolutionTaskID {
			return workspacecoord.MergeRequest{}, fmt.Errorf("%w: merge resolution task is required", orchestration.ErrInvalidEvent)
		}
		var taskTeam, taskState string
		if err := tx.QueryRowContext(ctx, `SELECT team_id, state FROM tasks WHERE task_id = ?`, string(transition.ResolutionTaskID)).Scan(&taskTeam, &taskState); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return workspacecoord.MergeRequest{}, fmt.Errorf("%w: resolution task %s", orchestration.ErrNotFound, transition.ResolutionTaskID)
			}
			return workspacecoord.MergeRequest{}, fmt.Errorf("inspect merge resolution task: %w", err)
		}
		if taskTeam != string(merge.TeamID) || taskState != string(orchestration.TaskCompleted) {
			return workspacecoord.MergeRequest{}, fmt.Errorf("%w: resolution task is not completed", orchestration.ErrInvalidEvent)
		}
	}
	if transition.Expected != 0 && merge.Version != transition.Expected {
		return workspacecoord.MergeRequest{}, fmt.Errorf("%w: merge expected version %d, current %d", orchestration.ErrSequenceConflict, transition.Expected, merge.Version)
	}
	if err := validateMergeTransition(merge.State, transition.Target); err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	now := time.Now().UTC()
	planDigest := merge.PlanDigest
	if transition.PlanDigest != "" {
		planDigest = transition.PlanDigest
	}
	if err := execCAS(ctx, tx, orchestration.ErrSequenceConflict, `UPDATE merge_queue SET state = ?, reason = ?, plan_digest = ?, version = version + 1, updated_at = ? WHERE merge_id = ? AND version = ?`, string(transition.Target), transition.Reason, planDigest, now.Format(time.RFC3339Nano), string(transition.MergeID), merge.Version); err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	sequence, err := currentOrchestrationSequence(ctx, tx, orchestration.StreamWorkspace, string(merge.TeamID))
	if err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	eventType := orchestration.EventMergeStarted
	if merge.State == workspacecoord.MergeConflicted && transition.Target == workspacecoord.MergePending {
		eventType = orchestration.EventMergeRequeued
	}
	switch transition.Target {
	case workspacecoord.MergeApplied:
		eventType = orchestration.EventMergeApplied
	case workspacecoord.MergeConflicted:
		eventType = orchestration.EventMergeConflicted
	case workspacecoord.MergeRejected:
		eventType = orchestration.EventMergeRejected
	case workspacecoord.MergeSuperseded:
		eventType = orchestration.EventMergeSuperseded
	}
	payload := map[string]any{"merge_id": transition.MergeID, "from": merge.State, "to": transition.Target, "reason": transition.Reason}
	if planDigest != "" {
		payload["plan_digest"] = planDigest
	}
	if transition.ResolutionTaskID != "" {
		payload["resolution_task_id"] = transition.ResolutionTaskID
	}
	if _, err := appendOrchestrationEvents(ctx, tx, orchestration.Stream{Type: orchestration.StreamWorkspace, ID: string(merge.TeamID), TeamID: merge.TeamID}, sequence, orchestration.PendingEvent{Type: eventType, ActorAgentID: transition.ActorAgentID, CorrelationID: transition.CorrelationID, Payload: orchestration.MustJSONPayload(payload)}); err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	updated, err := mergeByID(ctx, tx, transition.MergeID)
	if err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	if err := tx.Commit(); err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	return updated, nil
}

// LinkMergeResolutionTask records the durable task responsible for resolving
// a conflicted merge. Linking is idempotent for the same task and rejects
// attempts to replace an existing association.
func (store *OrchestrationStore) LinkMergeResolutionTask(ctx context.Context, mergeID orchestration.MergeID, taskID orchestration.TaskID) (workspacecoord.MergeRequest, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := mergeID.Validate(); err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	if err := taskID.Validate(); err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return workspacecoord.MergeRequest{}, fmt.Errorf("begin merge resolution link: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	merge, err := mergeByID(ctx, tx, mergeID)
	if errors.Is(err, sql.ErrNoRows) {
		return workspacecoord.MergeRequest{}, fmt.Errorf("%w: merge %s", orchestration.ErrNotFound, mergeID)
	}
	if err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	if merge.ResolutionTaskID == taskID {
		if err := tx.Commit(); err != nil {
			return workspacecoord.MergeRequest{}, err
		}
		return merge, nil
	}
	if merge.State != workspacecoord.MergeConflicted || merge.ResolutionTaskID != "" {
		return workspacecoord.MergeRequest{}, fmt.Errorf("%w: merge resolution task is already linked", orchestration.ErrIdempotencyConflict)
	}
	var taskTeam string
	if err := tx.QueryRowContext(ctx, `SELECT team_id FROM tasks WHERE task_id = ?`, string(taskID)).Scan(&taskTeam); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return workspacecoord.MergeRequest{}, fmt.Errorf("%w: resolution task %s", orchestration.ErrNotFound, taskID)
		}
		return workspacecoord.MergeRequest{}, fmt.Errorf("inspect merge resolution task: %w", err)
	}
	if taskTeam != string(merge.TeamID) {
		return workspacecoord.MergeRequest{}, fmt.Errorf("%w: resolution task belongs to another team", orchestration.ErrInvalidEvent)
	}
	if err := execCAS(ctx, tx, orchestration.ErrSequenceConflict, `UPDATE merge_queue SET resolution_task_id = ? WHERE merge_id = ? AND state = ? AND resolution_task_id = ''`, string(taskID), string(mergeID), string(workspacecoord.MergeConflicted)); err != nil {
		return workspacecoord.MergeRequest{}, fmt.Errorf("link merge resolution task: %w", err)
	}
	sequence, err := currentOrchestrationSequence(ctx, tx, orchestration.StreamWorkspace, string(merge.TeamID))
	if err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	if _, err := appendOrchestrationEvents(ctx, tx, orchestration.Stream{Type: orchestration.StreamWorkspace, ID: string(merge.TeamID), TeamID: merge.TeamID}, sequence, orchestration.PendingEvent{Type: orchestration.EventMergeResolutionLinked, ActorAgentID: merge.AgentID, CorrelationID: merge.CorrelationID, Payload: orchestration.MustJSONPayload(map[string]any{"merge_id": merge.ID, "resolution_task_id": taskID})}); err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	updated, err := mergeByID(ctx, tx, mergeID)
	if err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	if err := tx.Commit(); err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	return updated, nil
}

func validateMergeTransition(from, to workspacecoord.MergeState) error {
	allowed := map[workspacecoord.MergeState]map[workspacecoord.MergeState]bool{
		workspacecoord.MergePending:    {workspacecoord.MergeRunning: true, workspacecoord.MergeRejected: true, workspacecoord.MergeSuperseded: true},
		workspacecoord.MergeRunning:    {workspacecoord.MergeApplied: true, workspacecoord.MergeConflicted: true, workspacecoord.MergeRejected: true},
		workspacecoord.MergeConflicted: {workspacecoord.MergePending: true, workspacecoord.MergeRejected: true, workspacecoord.MergeSuperseded: true},
	}
	if !allowed[from][to] {
		return fmt.Errorf("%w: invalid merge transition %s -> %s", orchestration.ErrInvalidEvent, from, to)
	}
	return nil
}

func viewByID(ctx context.Context, query rowQuery, id orchestration.WorkspaceViewID) (workspacecoord.View, error) {
	return scanView(query.QueryRowContext(ctx, `SELECT view_id, team_id, agent_id, task_id, mode, workspace_root, root_path, base_revision, state, correlation_id, idempotency_key, created_at, updated_at, released_at FROM workspace_views WHERE view_id = ?`, string(id)))
}

func scanView(scanner rowScanner) (workspacecoord.View, error) {
	var view workspacecoord.View
	var viewID, teamID, agentID, taskID, mode, workspaceRoot, rootPath, baseRevision, state, correlationID, idempotencyKey, createdAt, updatedAt string
	var releasedAt sql.NullString
	if err := scanner.Scan(&viewID, &teamID, &agentID, &taskID, &mode, &workspaceRoot, &rootPath, &baseRevision, &state, &correlationID, &idempotencyKey, &createdAt, &updatedAt, &releasedAt); err != nil {
		return workspacecoord.View{}, err
	}
	view.ID = orchestration.WorkspaceViewID(viewID)
	view.TeamID = orchestration.TeamID(teamID)
	view.AgentID = orchestration.AgentID(agentID)
	view.TaskID = orchestration.TaskID(taskID)
	view.Mode = workspacecoord.ViewMode(mode)
	view.WorkspaceRoot = workspaceRoot
	view.RootPath = rootPath
	view.BaseRevision = baseRevision
	view.State = workspacecoord.ViewState(state)
	view.CorrelationID = correlationID
	view.IdempotencyKey = idempotencyKey
	var err error
	view.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return workspacecoord.View{}, err
	}
	view.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return workspacecoord.View{}, err
	}
	if releasedAt.Valid && releasedAt.String != "" {
		value, parseErr := time.Parse(time.RFC3339Nano, releasedAt.String)
		if parseErr != nil {
			return workspacecoord.View{}, parseErr
		}
		view.ReleasedAt = &value
	}
	return view, nil
}

func mergeByID(ctx context.Context, query rowQuery, id orchestration.MergeID) (workspacecoord.MergeRequest, error) {
	return scanMerge(query.QueryRowContext(ctx, `SELECT `+mergeColumns+` FROM merge_queue WHERE merge_id = ?`, string(id)))
}

// mergeColumns is the column list scanMerge reads, in order.
const mergeColumns = `merge_id, team_id, agent_id, task_id, view_id, base_revision, patch_artifact, resolution_task_id, state, reason, correlation_id, idempotency_key, version, created_at, updated_at, kind, candidate_digest, plan_digest, supersedes, superseded_by`

func scanMerge(scanner rowScanner) (workspacecoord.MergeRequest, error) {
	var merge workspacecoord.MergeRequest
	var mergeID, teamID, agentID, taskID, viewID, baseRevision, patchArtifact, resolutionTaskID, state, reason, correlationID, idempotencyKey, createdAt, updatedAt string
	var kind, supersedes, supersededBy string
	if err := scanner.Scan(&mergeID, &teamID, &agentID, &taskID, &viewID, &baseRevision, &patchArtifact, &resolutionTaskID, &state, &reason, &correlationID, &idempotencyKey, &merge.Version, &createdAt, &updatedAt, &kind, &merge.CandidateDigest, &merge.PlanDigest, &supersedes, &supersededBy); err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	merge.Kind = workspacecoord.MergeKind(kind)
	merge.Supersedes = orchestration.MergeID(supersedes)
	merge.SupersededBy = orchestration.MergeID(supersededBy)
	merge.ID = orchestration.MergeID(mergeID)
	merge.TeamID = orchestration.TeamID(teamID)
	merge.AgentID = orchestration.AgentID(agentID)
	merge.TaskID = orchestration.TaskID(taskID)
	merge.ViewID = orchestration.WorkspaceViewID(viewID)
	merge.BaseRevision = baseRevision
	merge.PatchArtifact = patchArtifact
	merge.ResolutionTaskID = orchestration.TaskID(resolutionTaskID)
	merge.State = workspacecoord.MergeState(state)
	merge.Reason = reason
	merge.CorrelationID = correlationID
	merge.IdempotencyKey = idempotencyKey
	var err error
	merge.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	merge.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return workspacecoord.MergeRequest{}, err
	}
	return merge, nil
}

var _ workspacecoord.Repository = (*OrchestrationStore)(nil)
var _ workspacecoord.ResolutionLinker = (*OrchestrationStore)(nil)

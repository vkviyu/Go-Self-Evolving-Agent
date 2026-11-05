package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

const defaultTaskClaimTTL = 30 * time.Minute

func (store *OrchestrationStore) ClaimTask(ctx context.Context, taskID orchestration.TaskID, agentID orchestration.AgentID, expected uint64) (orchestration.TaskSnapshot, error) {
	return store.TransitionTask(ctx, orchestration.TaskTransition{TaskID: taskID, Expected: expected, Target: orchestration.TaskClaimed, AssigneeAgentID: agentID, ActorAgentID: agentID, Reason: "claimed"})
}

// claimTaskForAgentTx is used by CreateChild so the child projection and its
// task claim commit atomically. The caller must already have inserted the
// Agent row in the surrounding transaction.
func claimTaskForAgentTx(ctx context.Context, tx *sql.Tx, taskID orchestration.TaskID, agentID orchestration.AgentID, actorAgentID orchestration.AgentID, correlationID string) error {
	task, err := taskByID(ctx, tx, taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: task %s", orchestration.ErrNotFound, taskID)
	}
	if err != nil {
		return err
	}
	if err := agentBelongsToTeam(ctx, tx, agentID, task.TeamID); err != nil {
		return err
	}
	if task.State != orchestration.TaskReady || task.AssigneeAgentID != "" {
		return fmt.Errorf("%w: task %s is not ready for Agent claim", orchestration.ErrTaskClaimConflict, taskID)
	}
	complete, err := dependenciesComplete(ctx, tx, task.ID)
	if err != nil {
		return err
	}
	if !complete {
		return fmt.Errorf("%w: task %s", orchestration.ErrTaskNotReady, task.ID)
	}
	now := time.Now().UTC()
	claimExpiresAt := now.Add(defaultTaskClaimTTL).Format(time.RFC3339Nano)
	if err := execCAS(ctx, tx, orchestration.ErrTaskClaimConflict, `UPDATE tasks SET state = ?, assignee_agent_id = ?, claim_expires_at = ?, version = version + 1, updated_at = ? WHERE task_id = ? AND version = ?`,
		string(orchestration.TaskClaimed), string(agentID), claimExpiresAt, now.Format(time.RFC3339Nano), string(task.ID), task.Version); err != nil {
		return fmt.Errorf("claim task for Agent: %w", err)
	}
	streamSequence, err := currentOrchestrationSequence(ctx, tx, orchestration.StreamTask, string(task.ID))
	if err != nil {
		return err
	}
	if _, err := appendOrchestrationEvents(ctx, tx, orchestration.Stream{Type: orchestration.StreamTask, ID: string(task.ID), TeamID: task.TeamID}, streamSequence,
		orchestration.PendingEvent{Type: orchestration.EventTaskUpdated, ActorAgentID: actorAgentID, CorrelationID: correlationID, Payload: orchestration.MustJSONPayload(map[string]any{
			"from": task.State, "to": orchestration.TaskClaimed, "assignee_agent_id": agentID, "reason": "claimed by spawned Agent",
		})},
	); err != nil {
		return fmt.Errorf("record task claim for Agent: %w", err)
	}
	return nil
}

func (store *OrchestrationStore) ReleaseTask(ctx context.Context, taskID orchestration.TaskID, agentID orchestration.AgentID, expected uint64) (orchestration.TaskSnapshot, error) {
	return store.TransitionTask(ctx, orchestration.TaskTransition{TaskID: taskID, Expected: expected, Target: orchestration.TaskReady, ActorAgentID: agentID, Reason: "released"})
}

// ReapExpiredTaskClaims returns expired claims to the ready queue. A claim is
// only reaped when its assigned Agent is no longer live, so a delayed
// scheduler cannot steal work from a running child.
func (store *OrchestrationStore) ReapExpiredTaskClaims(ctx context.Context, now time.Time) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("begin task claim reaper: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT task.task_id, task.team_id, task.assignee_agent_id, task.version
FROM tasks task
WHERE task.state = ? AND task.claim_expires_at <> '' AND task.claim_expires_at <= ?
  AND NOT EXISTS (
    SELECT 1 FROM agents owner
    WHERE owner.assigned_task_id = task.task_id
      AND owner.state IN (?, ?, ?, ?, ?)
  )
ORDER BY task.task_id ASC`, string(orchestration.TaskClaimed), now.Format(time.RFC3339Nano),
		string(orchestration.AgentCreated), string(orchestration.AgentQueued), string(orchestration.AgentRunning), string(orchestration.AgentWaiting), string(orchestration.AgentCanceling))
	if err != nil {
		return 0, fmt.Errorf("list expired task claims: %w", err)
	}
	type expiredClaim struct {
		id       orchestration.TaskID
		teamID   orchestration.TeamID
		assignee orchestration.AgentID
		version  uint64
	}
	claims := make([]expiredClaim, 0)
	for rows.Next() {
		var claim expiredClaim
		if err := rows.Scan(&claim.id, &claim.teamID, &claim.assignee, &claim.version); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan expired task claim: %w", err)
		}
		claims = append(claims, claim)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("iterate expired task claims: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close expired task claims: %w", err)
	}
	reaped := 0
	for _, claim := range claims {
		result, err := tx.ExecContext(ctx, `UPDATE tasks SET state = ?, assignee_agent_id = '', claim_expires_at = '', attempt = attempt + 1, version = version + 1, updated_at = ? WHERE task_id = ? AND state = ? AND version = ? AND claim_expires_at <> '' AND claim_expires_at <= ?`,
			string(orchestration.TaskReady), now.Format(time.RFC3339Nano), string(claim.id), string(orchestration.TaskClaimed), claim.version, now.Format(time.RFC3339Nano))
		if err != nil {
			return 0, fmt.Errorf("reap task claim %s: %w", claim.id, err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("inspect reaped claim %s: %w", claim.id, err)
		}
		if changed == 0 {
			continue
		}
		sequence, err := currentOrchestrationSequence(ctx, tx, orchestration.StreamTask, string(claim.id))
		if err != nil {
			return 0, err
		}
		if _, err := appendOrchestrationEvents(ctx, tx, orchestration.Stream{Type: orchestration.StreamTask, ID: string(claim.id), TeamID: claim.teamID}, sequence,
			orchestration.PendingEvent{Type: orchestration.EventTaskClaimExpired, ActorAgentID: claim.assignee, Payload: orchestration.MustJSONPayload(map[string]any{
				"from": orchestration.TaskClaimed, "to": orchestration.TaskReady, "assignee_agent_id": claim.assignee, "attempt": "incremented", "reason": orchestration.ErrTaskClaimExpired.Error(),
			})},
		); err != nil {
			return 0, fmt.Errorf("record expired task claim %s: %w", claim.id, err)
		}
		reaped++
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit task claim reaper: %w", err)
	}
	return reaped, nil
}

package sqlitestore

import (
	"context"
	"fmt"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

// authorizeTaskPlanActorTx authorizes plan changes using persisted Agent and
// Task ownership. Team membership alone grants no authority over a sibling's
// task. A root coordinates the Team; an assignee may refine its work; its
// Agent ancestors or an owner of an ancestor Task may manage the subtree.
func authorizeTaskPlanActorTx(ctx context.Context, query rowQuery, task orchestration.TaskSnapshot, actorID orchestration.AgentID) error {
	actor, err := agentByID(ctx, query, actorID)
	if err != nil {
		return fmt.Errorf("load task plan actor: %w", err)
	}
	if actor.TeamID != task.TeamID || isTerminalAgentState(actor.State) || actor.State == orchestration.AgentCanceling {
		return fmt.Errorf("%w: plan actor is not active in this team", orchestration.ErrInvalidEvent)
	}
	var root string
	if err := query.QueryRowContext(ctx, `SELECT root_agent_id FROM teams WHERE team_id = ?`, string(task.TeamID)).Scan(&root); err != nil {
		return err
	}
	if root == string(actor.ID) {
		return nil
	}
	// Walk Task ownership first, then Agent ancestry for those owners. Do not
	// infer current ownership from agents.assigned_task_id: it records the run
	// contract and can outlive a released or reassigned Task claim.
	var authorized bool
	if err := query.QueryRowContext(ctx, `WITH RECURSIVE
task_lineage(task_id, parent_task_id, assignee_agent_id) AS (
    SELECT task_id, parent_task_id, assignee_agent_id FROM tasks WHERE task_id = ? AND team_id = ?
    UNION SELECT t.task_id, t.parent_task_id, t.assignee_agent_id FROM tasks t JOIN task_lineage l ON t.task_id = l.parent_task_id WHERE t.team_id = ?
), agent_lineage(agent_id, parent_agent_id) AS (
    SELECT a.agent_id, a.parent_agent_id FROM agents a JOIN task_lineage t ON a.agent_id = t.assignee_agent_id WHERE a.team_id = ?
    UNION SELECT a.agent_id, a.parent_agent_id FROM agents a JOIN agent_lineage l ON a.agent_id = l.parent_agent_id WHERE a.team_id = ?
)
SELECT EXISTS(SELECT 1 FROM agent_lineage WHERE agent_id = ?)`, string(task.ID), string(task.TeamID), string(task.TeamID), string(task.TeamID), string(task.TeamID), string(actor.ID)).Scan(&authorized); err != nil {
		return err
	}
	if authorized {
		return nil
	}
	return fmt.Errorf("%w: only the root, task assignee or an owning ancestor may edit this plan", orchestration.ErrInvalidEvent)
}

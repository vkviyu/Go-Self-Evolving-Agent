package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

// Offline operations have different lifecycle gates. Keep their explicit query
// sets and error contracts separate; neither may mutate dependent owners.
func retentionDependencies(ctx context.Context, query rowQuery, id session.ID) ([]string, error) {
	var blockers []string
	var team, parent, state string
	var result []byte
	err := query.QueryRowContext(ctx, `SELECT a.team_id,a.parent_agent_id,a.state,a.result FROM agents a JOIN agent_threads t USING(agent_id) WHERE t.session_id=?`, string(id)).Scan(&team, &parent, &state, &result)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if parent != "" {
		var durableResult orchestration.AgentResult
		if (state != "completed" && state != "failed" && state != "canceled") || json.Unmarshal(result, &durableResult) != nil || durableResult.Validate() != nil {
			blockers = append(blockers, "child Agent has not durably finished with a valid result")
		}
	}
	type check struct {
		query, reason string
		args          []any
	}
	checks := []check{{`SELECT COUNT(*) FROM goal_runs WHERE session_id=? AND state='running'`, "running Goal", []any{string(id)}}}
	if team != "" {
		checks = append(checks,
			check{`SELECT COUNT(*) FROM agents WHERE team_id=? AND parent_agent_id<>'' AND state NOT IN ('completed','failed','canceled')`, "nonterminal child Agent", []any{team}},
			check{`SELECT COUNT(*) FROM tasks WHERE team_id=? AND state NOT IN ('completed','failed','canceled')`, "nonterminal Task", []any{team}},
			check{`SELECT COUNT(*) FROM goal_runs WHERE team_id=? AND state='running'`, "running team Goal", []any{team}},
			check{`SELECT COUNT(*) FROM flow_runs WHERE team_id=? AND state NOT IN ('completed','failed','canceled')`, "nonterminal team Flow", []any{team}},
			check{`SELECT COUNT(*) FROM wait_sets WHERE team_id=? AND state='pending'`, "pending wait", []any{team}},
			check{`SELECT COUNT(*) FROM merge_queue WHERE team_id=? AND state NOT IN ('applied','rejected','superseded')`, "unfinished merge", []any{team}},
		)
	}
	for _, item := range checks {
		var count int
		if err := query.QueryRowContext(ctx, item.query, item.args...).Scan(&count); err != nil {
			return nil, err
		}
		if count != 0 {
			blockers = append(blockers, item.reason)
		}
	}
	return blockers, nil
}

func offlineRoot(ctx context.Context, query rowQuery, id session.ID) (teamID, agentID string, err error) {
	var parent string
	err = query.QueryRowContext(ctx, `SELECT a.team_id, a.agent_id, a.parent_agent_id FROM agent_threads t JOIN agents a USING(agent_id) WHERE t.session_id = ?`, string(id)).Scan(&teamID, &agentID, &parent)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	} // Legacy root sessions have no orchestration row.
	if err != nil {
		return "", "", err
	}
	if parent != "" {
		return "", "", errors.New("offline abandonment is only supported for root sessions; child recovery belongs to Supervisor")
	}
	return teamID, agentID, nil
}

func guardOfflineDependencies(ctx context.Context, query rowQuery, id session.ID, teamID string) error {
	type dependencyCheck struct {
		query, reason string
		args          []any
	}
	checks := []dependencyCheck{
		{`SELECT COUNT(*) FROM flow_runs WHERE session_id = ? AND state NOT IN ('completed', 'failed', 'canceled')`, "nonterminal Flow in session", []any{string(id)}},
	}
	if teamID != "" {
		checks = append(checks,
			dependencyCheck{`SELECT COUNT(*) FROM agents WHERE team_id = ? AND parent_agent_id <> '' AND state NOT IN ('completed', 'failed', 'canceled')`, "nonterminal child Agent in team", []any{teamID}},
			dependencyCheck{`SELECT COUNT(*) FROM tasks WHERE team_id = ? AND state NOT IN ('completed', 'failed', 'canceled')`, "nonterminal task in team", []any{teamID}},
		)
	}
	for _, check := range checks {
		var count int
		if err := query.QueryRowContext(ctx, check.query, check.args...).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("%w: %s; resolve it through its owning lifecycle first", session.ErrRecoveryDependency, check.reason)
		}
	}
	return nil
}

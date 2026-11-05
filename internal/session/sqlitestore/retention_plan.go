package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"sort"

	"github.com/vkviyu/go-self-evolving-agent/internal/retention"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

// planRetention projects a stable audit using the caller's transaction. Preview
// and apply share this read-only path; it never repairs or backfills identity.
func planRetention(ctx context.Context, query readQuery, id session.ID) (retention.Plan, error) {
	plan := retention.Plan{SessionID: id, Consumers: []retention.Consumer{}, Blockers: []string{}}
	if id == "" || len(id) > 4096 {
		return plan, errors.New("retention requires an explicit session")
	}
	var err error
	plan.Sequence, err = currentSequence(ctx, query, id)
	if err != nil {
		return plan, err
	}
	plan.Blockers, err = retentionDependencies(ctx, query, id)
	if err != nil {
		return plan, err
	}
	turns, err := retentionTurns(ctx, query, id)
	if err != nil {
		return plan, err
	}
	// Include a retained-before-start admission and Goal rounds with no start.
	rows, err := query.QueryContext(ctx, `SELECT consumer_id FROM runtime_snapshot_refs WHERE kind='turn' AND scope=? UNION SELECT r.turn_id FROM goal_rounds r JOIN goal_runs g USING(goal_id) WHERE g.session_id=?`, string(id), string(id))
	if err != nil {
		return plan, err
	}
	for rows.Next() {
		var turnID string
		if err := rows.Scan(&turnID); err != nil {
			rows.Close()
			return plan, err
		}
		if turns[turnID] == nil {
			turns[turnID] = &retentionTurn{}
		}
	}
	if err := closeRetentionRows(rows); err != nil {
		return plan, err
	}
	if len(turns) > 10000 {
		return plan, errors.New("retention session exceeds 10000 turns; preserve until an indexed audit is available")
	}
	for turnID, state := range turns {
		consumer := retention.Consumer{Reference: tool.SnapshotReference{Kind: "turn", Scope: string(id), ID: turnID}, SnapshotID: state.snapshot}
		closed, err := loadRetentionReference(ctx, query, &consumer)
		if err != nil {
			return plan, err
		}
		if closed || state.closed {
			continue
		}
		var goalState string
		err = query.QueryRowContext(ctx, `SELECT g.state FROM goal_runs g JOIN goal_rounds r USING(goal_id) WHERE g.session_id=? AND r.turn_id=?`, string(id), turnID).Scan(&goalState)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return plan, err
		}
		terminal := state.started && state.completed
		if err == nil {
			terminal = goalState == "completed" || goalState == "failed" || goalState == "canceled" || goalState == "blocked" || goalState == "exhausted"
		}
		if !terminal {
			plan.Blockers = append(plan.Blockers, "turn "+turnID+": execution or Goal is unfinished; inspect effects and explicitly abandon if appropriate")
			continue
		}
		plan.Consumers = append(plan.Consumers, consumer)
	}
	rows, err = query.QueryContext(ctx, `SELECT team_id,idempotency_key,runtime_snapshot_id,state FROM flow_runs WHERE session_id=? ORDER BY team_id,idempotency_key`, string(id))
	if err != nil {
		return plan, err
	}
	type flowCandidate struct {
		consumer retention.Consumer
		state    string
	}
	var flows []flowCandidate
	for rows.Next() {
		candidate := flowCandidate{consumer: retention.Consumer{Reference: tool.SnapshotReference{Kind: "flow"}}}
		if err := rows.Scan(&candidate.consumer.Reference.Scope, &candidate.consumer.Reference.ID, &candidate.consumer.SnapshotID, &candidate.state); err != nil {
			rows.Close()
			return plan, err
		}
		flows = append(flows, candidate)
	}
	if err := closeRetentionRows(rows); err != nil {
		return plan, err
	}
	for _, flow := range flows {
		closed, err := loadRetentionReference(ctx, query, &flow.consumer)
		if err != nil {
			return plan, err
		}
		if closed {
			continue
		}
		if flow.state != "completed" && flow.state != "failed" && flow.state != "canceled" {
			plan.Blockers = append(plan.Blockers, "flow "+flow.consumer.Reference.ID+": unfinished")
			continue
		}
		plan.Consumers = append(plan.Consumers, flow.consumer)
	}
	sort.Slice(plan.Consumers, func(i, j int) bool {
		a, b := plan.Consumers[i].Reference, plan.Consumers[j].Reference
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Scope != b.Scope {
			return a.Scope < b.Scope
		}
		return a.ID < b.ID
	})
	sort.Strings(plan.Blockers)
	plan.ID = plan.Digest()
	return plan, nil
}

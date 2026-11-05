package sqlitestore

import (
	"context"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

func (store *OrchestrationStore) PendingRounds(ctx context.Context, id session.ID) ([]agentgoal.Round, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT goal_id, current_round FROM goal_runs WHERE session_id = ? AND state = 'running' ORDER BY started_at, goal_id`, string(id))
	if err != nil {
		return nil, err
	}
	var pending []agentgoal.Round
	for rows.Next() {
		var round agentgoal.Round
		if err := rows.Scan(&round.GoalID, &round.Number); err != nil {
			rows.Close()
			return nil, err
		}
		pending = append(pending, round)
	}
	err = rows.Err()
	if closeErr := rows.Close(); err != nil {
		return nil, err
	} else if closeErr != nil {
		return nil, closeErr
	}
	for index, round := range pending {
		if round.Number == 0 {
			continue
		}
		pending[index], err = roundByNumber(ctx, store.db, round.GoalID, round.Number)
		if err != nil {
			return nil, err
		}
	}
	return pending, nil
}

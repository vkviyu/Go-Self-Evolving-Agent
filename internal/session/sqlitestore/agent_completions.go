package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

// PrepareAgentCompletion freezes a runner result before any coordinator-side
// task or workspace settlement. It is idempotent by Agent+Run and fences a
// late result from a later run.
func (store *OrchestrationStore) PrepareAgentCompletion(ctx context.Context, completion orchestration.AgentCompletion) (orchestration.AgentCompletion, error) {
	if completion.AgentID == "" || completion.RunID == "" {
		return orchestration.AgentCompletion{}, fmt.Errorf("%w: completion identity is required", orchestration.ErrInvalidEvent)
	}
	if err := completion.Result.Validate(); err != nil {
		return orchestration.AgentCompletion{}, err
	}
	encoded, err := json.Marshal(completion.Result)
	if err != nil {
		return orchestration.AgentCompletion{}, fmt.Errorf("encode Agent completion: %w", err)
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return orchestration.AgentCompletion{}, err
	}
	defer func() { _ = tx.Rollback() }()
	agent, err := agentByID(ctx, tx, completion.AgentID)
	if err != nil {
		return orchestration.AgentCompletion{}, err
	}
	if agent.CurrentRunID != completion.RunID {
		return orchestration.AgentCompletion{}, fmt.Errorf("%w: stale completion Run ID", orchestration.ErrAgentStateConflict)
	}
	hash := sha256Hex(encoded)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var existing string
	lookupErr := tx.QueryRowContext(ctx, `SELECT result_sha FROM agent_completions WHERE agent_id = ? AND run_id = ?`, string(completion.AgentID), string(completion.RunID)).Scan(&existing)
	if lookupErr == nil {
		if existing != hash {
			return orchestration.AgentCompletion{}, fmt.Errorf("%w: Agent completion %s/%s", orchestration.ErrIdempotencyConflict, completion.AgentID, completion.RunID)
		}
		return completion, tx.Commit()
	}
	if lookupErr != sql.ErrNoRows {
		return orchestration.AgentCompletion{}, fmt.Errorf("inspect Agent completion: %w", lookupErr)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO agent_completions(agent_id, run_id, result, result_sha, settled, created_at) VALUES(?, ?, ?, ?, 0, ?)`, string(completion.AgentID), string(completion.RunID), encoded, hash, now)
	if err != nil {
		return orchestration.AgentCompletion{}, fmt.Errorf("persist Agent completion: %w", err)
	}
	return completion, tx.Commit()
}

func (store *OrchestrationStore) PendingAgentCompletions(ctx context.Context, teamID orchestration.TeamID) ([]orchestration.AgentCompletion, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT c.agent_id, c.run_id, c.result, c.result_sha FROM agent_completions c JOIN agents a ON a.agent_id = c.agent_id WHERE a.team_id = ? AND c.settled = 0 ORDER BY c.created_at, c.agent_id`, string(teamID))
	if err != nil {
		return nil, fmt.Errorf("list pending Agent completions: %w", err)
	}
	defer rows.Close()
	var result []orchestration.AgentCompletion
	for rows.Next() {
		var agentID, runID, checksum string
		var payload []byte
		var value orchestration.AgentResult
		if err := rows.Scan(&agentID, &runID, &payload, &checksum); err != nil {
			return nil, err
		}
		if sha256Hex(payload) != checksum {
			return nil, fmt.Errorf("%w: corrupt Agent completion %s/%s", orchestration.ErrInvalidEvent, agentID, runID)
		}
		if err := json.Unmarshal(payload, &value); err != nil {
			return nil, fmt.Errorf("decode pending Agent completion: %w", err)
		}
		if err := value.Validate(); err != nil {
			return nil, fmt.Errorf("invalid pending Agent completion: %w", err)
		}
		result = append(result, orchestration.AgentCompletion{AgentID: orchestration.AgentID(agentID), RunID: orchestration.RunID(runID), Result: value})
	}
	return result, rows.Err()
}

func (store *OrchestrationStore) SettleAgentCompletion(ctx context.Context, agentID orchestration.AgentID, runID orchestration.RunID) error {
	result, err := store.db.ExecContext(ctx, `UPDATE agent_completions SET settled = 1, settled_at = ? WHERE agent_id = ? AND run_id = ? AND settled = 0`, time.Now().UTC().Format(time.RFC3339Nano), string(agentID), string(runID))
	if err != nil {
		return fmt.Errorf("settle Agent completion: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		var exists int
		if err := store.db.QueryRowContext(ctx, `SELECT 1 FROM agent_completions WHERE agent_id = ? AND run_id = ?`, string(agentID), string(runID)).Scan(&exists); err != nil {
			if err == sql.ErrNoRows {
				return fmt.Errorf("%w: Agent completion", orchestration.ErrNotFound)
			}
			return fmt.Errorf("inspect settled Agent completion: %w", err)
		}
	}
	return nil
}

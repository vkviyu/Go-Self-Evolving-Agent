package sqlitestore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentflow"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

func hashFlowRequest(request agentflow.RunRequest) string {
	// Generation is process-local debug metadata once a content ID is present.
	if request.RuntimeSnapshotID != "" {
		request.RegistryGeneration = 0
	}
	canonical := struct {
		Definition            string                `json:"definition"`
		DefinitionRevision    string                `json:"definition_revision"`
		DefinitionFingerprint string                `json:"definition_fingerprint"`
		SessionID             string                `json:"session_id"`
		TeamID                orchestration.TeamID  `json:"team_id"`
		AgentID               orchestration.AgentID `json:"agent_id"`
		Input                 agentflow.Value       `json:"input"`
		RegistryGeneration    uint64                `json:"registry_generation"`
		RuntimeSnapshotID     string                `json:"runtime_snapshot_id,omitempty"`
	}{request.Definition, request.DefinitionRevision, request.DefinitionFingerprint, string(request.SessionID), request.TeamID, request.AgentID, request.Input, request.RegistryGeneration, request.RuntimeSnapshotID}
	encoded, _ := json.Marshal(canonical)
	return sha256Hex(encoded)
}

func (store *OrchestrationStore) CreateRun(ctx context.Context, request agentflow.RunRequest) (agentflow.Run, error) {
	if err := request.Validate(); err != nil {
		return agentflow.Run{}, err
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return agentflow.Run{}, fmt.Errorf("begin Flow run: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	requestHash := hashFlowRequest(request)
	var existingID, existingHash string
	err = tx.QueryRowContext(ctx, `SELECT flow_id, request_hash FROM flow_runs WHERE team_id = ? AND idempotency_key = ?`, string(request.TeamID), request.IdempotencyKey).Scan(&existingID, &existingHash)
	if err == nil {
		if existingHash != requestHash {
			return agentflow.Run{}, fmt.Errorf("%w: Flow key %q", orchestration.ErrIdempotencyConflict, request.IdempotencyKey)
		}
		run, loadErr := flowRunByID(ctx, tx, existingID)
		if loadErr != nil {
			return agentflow.Run{}, loadErr
		}
		if err := tx.Commit(); err != nil {
			return agentflow.Run{}, err
		}
		return run, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return agentflow.Run{}, err
	}
	input, err := json.Marshal(request.Input)
	if err != nil {
		return agentflow.Run{}, err
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO flow_runs(flow_id, idempotency_key, request_hash, definition, definition_revision, definition_fingerprint, session_id, team_id, agent_id, state, input, registry_generation, runtime_snapshot_id, version, started_at, updated_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		request.ID, request.IdempotencyKey, requestHash, request.Definition, request.DefinitionRevision, request.DefinitionFingerprint, string(request.SessionID), string(request.TeamID), string(request.AgentID), string(agentflow.StateRunning), input, request.RegistryGeneration, request.RuntimeSnapshotID, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		return agentflow.Run{}, fmt.Errorf("insert Flow run: %w", err)
	}
	if _, err := appendOrchestrationEvents(ctx, tx, orchestration.Stream{Type: orchestration.StreamFlow, ID: request.ID, TeamID: request.TeamID}, orchestration.AnySequence,
		orchestration.PendingEvent{Type: orchestration.EventFlowStarted, ActorAgentID: request.AgentID, CorrelationID: request.ID, IdempotencyKey: request.IdempotencyKey, Payload: orchestration.MustJSONPayload(map[string]any{
			"definition": request.Definition, "definition_revision": request.DefinitionRevision,
			"definition_fingerprint": request.DefinitionFingerprint, "session_id": request.SessionID, "registry_generation": request.RegistryGeneration, "runtime_snapshot_id": request.RuntimeSnapshotID,
		})}); err != nil {
		return agentflow.Run{}, err
	}
	run, err := flowRunByID(ctx, tx, request.ID)
	if err != nil {
		return agentflow.Run{}, err
	}
	if err := tx.Commit(); err != nil {
		return agentflow.Run{}, fmt.Errorf("commit Flow run: %w", err)
	}
	return run, nil
}

func (store *OrchestrationStore) GetRun(ctx context.Context, runID string) (agentflow.Run, error) {
	return flowRunByID(ctx, store.db, runID)
}

func (store *OrchestrationStore) StartStep(ctx context.Context, runID, stepID, inputHash string) (agentflow.Step, error) {
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return agentflow.Step{}, fmt.Errorf("begin Flow step: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	run, err := flowRunByID(ctx, tx, runID)
	if err != nil {
		return agentflow.Step{}, err
	}
	if run.State != agentflow.StateRunning {
		return agentflow.Step{}, fmt.Errorf("Flow %s is not running", runID)
	}
	step, loadErr := flowStepByID(ctx, tx, runID, stepID)
	if loadErr == nil {
		if step.InputHash != inputHash {
			return agentflow.Step{}, fmt.Errorf("%w: %s/%s", agentflow.ErrStepConflict, runID, stepID)
		}
		if step.State == agentflow.StepFailed {
			now := time.Now().UTC()
			if _, err := tx.ExecContext(ctx, `UPDATE flow_steps SET state = ?, error = '', outputs = X'5B5D', attempt = attempt + 1, updated_at = ?, completed_at = NULL WHERE flow_id = ? AND step_id = ?`, string(agentflow.StepRunning), now.Format(time.RFC3339Nano), runID, stepID); err != nil {
				return agentflow.Step{}, err
			}
			step, err = flowStepByID(ctx, tx, runID, stepID)
			if err != nil {
				return agentflow.Step{}, err
			}
		}
		if err := tx.Commit(); err != nil {
			return agentflow.Step{}, err
		}
		return step, nil
	}
	if !errors.Is(loadErr, orchestration.ErrNotFound) {
		return agentflow.Step{}, loadErr
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO flow_steps(flow_id, step_id, input_hash, state, attempt, started_at, updated_at) VALUES(?, ?, ?, ?, 1, ?, ?)`, runID, stepID, inputHash, string(agentflow.StepRunning), now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		return agentflow.Step{}, fmt.Errorf("insert Flow step: %w", err)
	}
	if err := appendFlowStepEvent(ctx, tx, run, orchestration.EventFlowStepStarted, stepID, map[string]any{"attempt": 1}); err != nil {
		return agentflow.Step{}, err
	}
	step, err = flowStepByID(ctx, tx, runID, stepID)
	if err != nil {
		return agentflow.Step{}, err
	}
	if err := tx.Commit(); err != nil {
		return agentflow.Step{}, err
	}
	return step, nil
}

func (store *OrchestrationStore) FinishStep(ctx context.Context, runID, stepID string, outputs []agentflow.Value) (agentflow.Step, error) {
	encoded, err := json.Marshal(outputs)
	if err != nil {
		return agentflow.Step{}, err
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return agentflow.Step{}, fmt.Errorf("begin Flow step completion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	run, err := flowRunByID(ctx, tx, runID)
	if err != nil {
		return agentflow.Step{}, err
	}
	step, err := flowStepByID(ctx, tx, runID, stepID)
	if err != nil {
		return agentflow.Step{}, err
	}
	if step.State == agentflow.StepCompleted {
		stored, _ := json.Marshal(step.Outputs)
		if !bytes.Equal(stored, encoded) {
			return agentflow.Step{}, fmt.Errorf("%w: completed step %s/%s", orchestration.ErrIdempotencyConflict, runID, stepID)
		}
		if err := tx.Commit(); err != nil {
			return agentflow.Step{}, err
		}
		return step, nil
	}
	if step.State != agentflow.StepRunning {
		return agentflow.Step{}, fmt.Errorf("Flow step %s/%s is not running", runID, stepID)
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE flow_steps SET state = ?, outputs = ?, error = '', updated_at = ?, completed_at = ? WHERE flow_id = ? AND step_id = ?`, string(agentflow.StepCompleted), encoded, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), runID, stepID); err != nil {
		return agentflow.Step{}, err
	}
	if err := appendFlowStepEvent(ctx, tx, run, orchestration.EventFlowStepCompleted, stepID, map[string]any{"output_count": len(outputs)}); err != nil {
		return agentflow.Step{}, err
	}
	step, err = flowStepByID(ctx, tx, runID, stepID)
	if err != nil {
		return agentflow.Step{}, err
	}
	if err := tx.Commit(); err != nil {
		return agentflow.Step{}, err
	}
	return step, nil
}

func (store *OrchestrationStore) FailStep(ctx context.Context, runID, stepID, reason string) error {
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	run, err := flowRunByID(ctx, tx, runID)
	if err != nil {
		return err
	}
	step, err := flowStepByID(ctx, tx, runID, stepID)
	if err != nil {
		return err
	}
	if step.State == agentflow.StepFailed && step.Error == reason {
		return tx.Commit()
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE flow_steps SET state = ?, error = ?, updated_at = ?, completed_at = ? WHERE flow_id = ? AND step_id = ?`, string(agentflow.StepFailed), reason, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), runID, stepID); err != nil {
		return err
	}
	if err := appendFlowStepEvent(ctx, tx, run, orchestration.EventFlowStepFailed, stepID, map[string]any{"error": reason}); err != nil {
		return err
	}
	return tx.Commit()
}

func (store *OrchestrationStore) FinishRun(ctx context.Context, runID string, state agentflow.State, outputs []agentflow.Value, reason string) (agentflow.Run, error) {
	if state != agentflow.StateCompleted && state != agentflow.StateFailed && state != agentflow.StateCanceled {
		return agentflow.Run{}, fmt.Errorf("unsupported terminal Flow state %q", state)
	}
	encoded, err := json.Marshal(outputs)
	if err != nil {
		return agentflow.Run{}, err
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return agentflow.Run{}, err
	}
	defer func() { _ = tx.Rollback() }()
	run, err := flowRunByID(ctx, tx, runID)
	if err != nil {
		return agentflow.Run{}, err
	}
	if run.State != agentflow.StateRunning {
		stored, _ := json.Marshal(run.Outputs)
		if run.State != state || run.Error != reason || !bytes.Equal(stored, encoded) {
			return agentflow.Run{}, fmt.Errorf("%w: terminal Flow %s", orchestration.ErrIdempotencyConflict, runID)
		}
		if err := tx.Commit(); err != nil {
			return agentflow.Run{}, err
		}
		return run, nil
	}
	now := time.Now().UTC()
	if err := execCAS(ctx, tx, orchestration.ErrSequenceConflict, `UPDATE flow_runs SET state = ?, outputs = ?, error = ?, version = version + 1, updated_at = ?, completed_at = ? WHERE flow_id = ? AND version = ?`, string(state), encoded, reason, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), runID, run.Version); err != nil {
		return agentflow.Run{}, err
	}
	eventType := orchestration.EventFlowCompleted
	switch state {
	case agentflow.StateFailed:
		eventType = orchestration.EventFlowFailed
	case agentflow.StateCanceled:
		eventType = orchestration.EventFlowCanceled
	}
	if _, err := appendOrchestrationEvents(ctx, tx, orchestration.Stream{Type: orchestration.StreamFlow, ID: run.ID, TeamID: run.TeamID}, orchestration.AnySequence,
		orchestration.PendingEvent{Type: eventType, ActorAgentID: run.AgentID, CorrelationID: run.ID, Payload: orchestration.MustJSONPayload(map[string]any{"output_count": len(outputs), "error": reason})}); err != nil {
		return agentflow.Run{}, err
	}
	run, err = flowRunByID(ctx, tx, runID)
	if err != nil {
		return agentflow.Run{}, err
	}
	if err := tx.Commit(); err != nil {
		return agentflow.Run{}, err
	}
	return run, nil
}

func (store *OrchestrationStore) SessionAgent(ctx context.Context, runID, sessionName string) (orchestration.AgentID, bool, error) {
	var agentID string
	err := store.db.QueryRowContext(ctx, `SELECT agent_id FROM flow_agent_sessions WHERE flow_id = ? AND session_name = ?`, runID, sessionName).Scan(&agentID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return orchestration.AgentID(agentID), err == nil, err
}

func (store *OrchestrationStore) BeginSessionStage(ctx context.Context, runID, sessionName, stepID string) (agentflow.SessionStage, error) {
	if sessionName == "" || stepID == "" {
		return agentflow.SessionStage{}, errors.New("Flow Agent session stage requires session and step names")
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return agentflow.SessionStage{}, err
	}
	defer func() { _ = tx.Rollback() }()
	run, err := flowRunByID(ctx, tx, runID)
	if err != nil {
		return agentflow.SessionStage{}, err
	}
	if run.State != agentflow.StateRunning {
		return agentflow.SessionStage{}, fmt.Errorf("Flow %s is not running", runID)
	}
	stage := agentflow.SessionStage{RunID: runID, SessionName: sessionName, StepID: stepID}
	var action string
	err = tx.QueryRowContext(ctx, `SELECT action FROM flow_agent_stages WHERE flow_id = ? AND session_name = ? AND step_id = ?`, runID, sessionName, stepID).Scan(&action)
	if err == nil {
		stage.Action = agentflow.SessionAction(action)
		if err := tx.Commit(); err != nil {
			return agentflow.SessionStage{}, err
		}
		return stage, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return agentflow.SessionStage{}, err
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM flow_agent_sessions WHERE flow_id = ? AND session_name = ?)`, runID, sessionName).Scan(&exists); err != nil {
		return agentflow.SessionStage{}, err
	}
	stage.Action = agentflow.SessionSpawn
	if exists {
		stage.Action = agentflow.SessionContinue
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO flow_agent_stages(flow_id, session_name, step_id, action, created_at) VALUES(?, ?, ?, ?, ?)`, runID, sessionName, stepID, string(stage.Action), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return agentflow.SessionStage{}, err
	}
	if err := tx.Commit(); err != nil {
		return agentflow.SessionStage{}, err
	}
	return stage, nil
}

func (store *OrchestrationStore) BindSessionAgent(ctx context.Context, runID, sessionName string, agentID orchestration.AgentID) error {
	if err := agentID.Validate(); err != nil {
		return err
	}
	if sessionName == "" {
		return fmt.Errorf("%w: Flow Agent session name is required", orchestration.ErrInvalidEvent)
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	run, err := flowRunByID(ctx, tx, runID)
	if err != nil {
		return err
	}
	if err := agentBelongsToTeam(ctx, tx, agentID, run.TeamID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO flow_agent_sessions(flow_id, session_name, agent_id, created_at) VALUES(?, ?, ?, ?) ON CONFLICT(flow_id, session_name) DO NOTHING`, runID, sessionName, string(agentID), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	var stored string
	if err := tx.QueryRowContext(ctx, `SELECT agent_id FROM flow_agent_sessions WHERE flow_id = ? AND session_name = ?`, runID, sessionName).Scan(&stored); err != nil {
		return recordError(err, "Flow Agent session", runID+"/"+sessionName)
	}
	if stored != string(agentID) {
		return fmt.Errorf("%w: Flow Agent session %s/%s", orchestration.ErrIdempotencyConflict, runID, sessionName)
	}
	return tx.Commit()
}

func appendFlowStepEvent(ctx context.Context, tx *sql.Tx, run agentflow.Run, eventType orchestration.EventType, stepID string, details map[string]any) error {
	details["step_id"] = stepID
	_, err := appendOrchestrationEvents(ctx, tx, orchestration.Stream{Type: orchestration.StreamFlow, ID: run.ID, TeamID: run.TeamID}, orchestration.AnySequence,
		orchestration.PendingEvent{Type: eventType, ActorAgentID: run.AgentID, CorrelationID: run.ID, Payload: orchestration.MustJSONPayload(details)})
	return err
}

func flowRunByID(ctx context.Context, query rowQuery, runID string) (agentflow.Run, error) {
	var run agentflow.Run
	var sessionID, teamID, agentID, state, startedAt, updatedAt string
	var input, outputs []byte
	var completedAt sql.NullString
	err := query.QueryRowContext(ctx, `SELECT flow_id, idempotency_key, definition, definition_revision, definition_fingerprint, session_id, team_id, agent_id, state, input, outputs, error, registry_generation, runtime_snapshot_id, version, started_at, updated_at, completed_at FROM flow_runs WHERE flow_id = ?`, runID).Scan(
		&run.ID, &run.IdempotencyKey, &run.Definition, &run.DefinitionRevision, &run.DefinitionFingerprint, &sessionID, &teamID, &agentID, &state, &input, &outputs, &run.Error, &run.RegistryGeneration, &run.RuntimeSnapshotID, &run.Version, &startedAt, &updatedAt, &completedAt)
	if err != nil {
		return agentflow.Run{}, recordError(err, "Flow", runID)
	}
	run.SessionID = session.ID(sessionID)
	run.TeamID, run.AgentID, run.State = orchestration.TeamID(teamID), orchestration.AgentID(agentID), agentflow.State(state)
	if err := json.Unmarshal(input, &run.Input); err != nil {
		return agentflow.Run{}, err
	}
	if err := json.Unmarshal(outputs, &run.Outputs); err != nil {
		return agentflow.Run{}, err
	}
	if run.StartedAt, err = time.Parse(time.RFC3339Nano, startedAt); err != nil {
		return agentflow.Run{}, err
	}
	if run.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return agentflow.Run{}, err
	}
	if completedAt.Valid {
		value, parseErr := time.Parse(time.RFC3339Nano, completedAt.String)
		if parseErr != nil {
			return agentflow.Run{}, parseErr
		}
		run.CompletedAt = &value
	}
	return run, nil
}

func flowStepByID(ctx context.Context, query rowQuery, runID, stepID string) (agentflow.Step, error) {
	var step agentflow.Step
	var state, startedAt, updatedAt string
	var outputs []byte
	var completedAt sql.NullString
	err := query.QueryRowContext(ctx, `SELECT flow_id, step_id, input_hash, state, outputs, error, attempt, started_at, updated_at, completed_at FROM flow_steps WHERE flow_id = ? AND step_id = ?`, runID, stepID).Scan(
		&step.RunID, &step.ID, &step.InputHash, &state, &outputs, &step.Error, &step.Attempt, &startedAt, &updatedAt, &completedAt)
	if err != nil {
		return agentflow.Step{}, recordError(err, "Flow step", runID+"/"+stepID)
	}
	step.State = agentflow.StepState(state)
	if err := json.Unmarshal(outputs, &step.Outputs); err != nil {
		return agentflow.Step{}, err
	}
	if step.StartedAt, err = time.Parse(time.RFC3339Nano, startedAt); err != nil {
		return agentflow.Step{}, err
	}
	if step.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return agentflow.Step{}, err
	}
	if completedAt.Valid {
		value, parseErr := time.Parse(time.RFC3339Nano, completedAt.String)
		if parseErr != nil {
			return agentflow.Step{}, parseErr
		}
		step.CompletedAt = &value
	}
	return step, nil
}

var _ agentflow.Store = (*OrchestrationStore)(nil)

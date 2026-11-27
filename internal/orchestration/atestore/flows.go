package atestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentflow"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

var _ agentflow.Store = (*Store)(nil)

type flowRequestIndex struct {
	ID   string
	Hash string
}

// flowKey encodes an ordered set of flow key components as a JSON array to preserve their
// boundaries.
func flowKey(parts ...string) string {
	raw, _ := json.Marshal(parts)
	return string(raw)
}

func flowHashRequest(request agentflow.RunRequest) string {
	if request.RuntimeSnapshotID != "" {
		request.RegistryGeneration = 0
	}
	canonical := struct {
		Definition            string                         `json:"definition"`
		DefinitionRevision    string                         `json:"definition_revision"`
		DefinitionFingerprint string                         `json:"definition_fingerprint"`
		SessionID             string                         `json:"session_id"`
		TeamID                orchestration.TeamID           `json:"team_id"`
		AgentID               orchestration.AgentID          `json:"agent_id"`
		Input                 agentflow.Value                `json:"input"`
		RegistryGeneration    uint64                         `json:"registry_generation"`
		RuntimeSnapshotID     string                         `json:"runtime_snapshot_id,omitempty"`
		Execution             orchestration.ExecutionBinding `json:"execution,omitempty"`
	}{request.Definition, request.DefinitionRevision, request.DefinitionFingerprint, string(request.SessionID), request.TeamID, request.AgentID, request.Input, request.RegistryGeneration, request.RuntimeSnapshotID, request.Execution}
	raw, _ := json.Marshal(canonical)
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

// CreateRun records the flow request and returns the corresponding run.
func (store *Store) CreateRun(ctx context.Context, request agentflow.RunRequest) (run agentflow.Run, err error) {
	if err = request.Validate(); err != nil {
		return
	}
	err = store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		key := flowKey(string(request.TeamID), request.IdempotencyKey)
		hash := flowHashRequest(request)
		index, err := getRecord[flowRequestIndex](tx, "flow-request", key)
		if err == nil {
			if index.Hash != hash {
				return fmt.Errorf("%w: Flow key %q", orchestration.ErrIdempotencyConflict, request.IdempotencyKey)
			}
			run, err = getRecord[agentflow.Run](tx, "flow", index.ID)
			return err
		}
		if !errors.Is(err, orchestration.ErrNotFound) {
			return err
		}
		// Preserve SQL foreign keys; creation does not require Agent membership.
		if _, err := getRecord[orchestration.TeamSnapshot](tx, "team", string(request.TeamID)); err != nil {
			return err
		}
		if _, err := getRecord[orchestration.AgentSnapshot](tx, "agent", string(request.AgentID)); err != nil {
			return err
		}
		now := time.Now().UTC()
		run = agentflow.Run{ID: request.ID, IdempotencyKey: request.IdempotencyKey,
			Definition: request.Definition, DefinitionRevision: request.DefinitionRevision,
			DefinitionFingerprint: request.DefinitionFingerprint, SessionID: request.SessionID,
			TeamID: request.TeamID, AgentID: request.AgentID, State: agentflow.StateRunning,
			Input: request.Input, Outputs: []agentflow.Value{}, RegistryGeneration: request.RegistryGeneration,
			RuntimeSnapshotID: request.RuntimeSnapshotID, Execution: request.Execution,
			Version: 1, StartedAt: now, UpdatedAt: now}
		if err := createRecord(tx, "flow", run.ID, run); err != nil {
			return err
		}
		if err := createRecord(tx, "flow-request", key, flowRequestIndex{ID: run.ID, Hash: hash}); err != nil {
			return err
		}
		_, err = appendEvents(tx, orchestration.Stream{Type: orchestration.StreamFlow, ID: run.ID, TeamID: run.TeamID}, orchestration.AnySequence,
			orchestration.PendingEvent{Type: orchestration.EventFlowStarted, ActorAgentID: run.AgentID, CorrelationID: run.ID, IdempotencyKey: request.IdempotencyKey,
				Payload: orchestration.MustJSONPayload(map[string]any{"definition": request.Definition,
					"definition_revision": request.DefinitionRevision, "definition_fingerprint": request.DefinitionFingerprint,
					"session_id": request.SessionID, "registry_generation": request.RegistryGeneration, "runtime_snapshot_id": request.RuntimeSnapshotID})})
		return err
	})
	return
}

// GetRun reads the persisted flow run snapshot by its run ID.
func (store *Store) GetRun(ctx context.Context, id string) (run agentflow.Run, err error) {
	err = store.authority.View(ctx, func(tx *ateauthority.Tx) error {
		run, err = getRecord[agentflow.Run](tx, "flow", id)
		return err
	})
	return
}

func flowAppendStepEvent(tx *ateauthority.Tx, run agentflow.Run, eventType orchestration.EventType, stepID string, details map[string]any) error {
	details["step_id"] = stepID
	_, err := appendEvents(tx, orchestration.Stream{Type: orchestration.StreamFlow, ID: run.ID, TeamID: run.TeamID}, orchestration.AnySequence,
		orchestration.PendingEvent{Type: eventType, ActorAgentID: run.AgentID, CorrelationID: run.ID, Payload: orchestration.MustJSONPayload(details)})
	return err
}

// StartStep checks the stored input hash when the same step is started again.
func (store *Store) StartStep(ctx context.Context, runID, stepID, inputHash string) (step agentflow.Step, err error) {
	err = store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		run, err := getRecord[agentflow.Run](tx, "flow", runID)
		if err != nil {
			return err
		}
		if run.State != agentflow.StateRunning {
			return fmt.Errorf("Flow %s is not running", runID)
		}
		key := flowKey(runID, stepID)
		step, err = getRecord[agentflow.Step](tx, "flow-step", key)
		if err == nil {
			if step.InputHash != inputHash {
				return fmt.Errorf("%w: %s/%s", agentflow.ErrStepConflict, runID, stepID)
			}
			if step.State == agentflow.StepFailed {
				step.State, step.Error, step.Outputs = agentflow.StepRunning, "", []agentflow.Value{}
				step.Attempt++
				step.UpdatedAt, step.CompletedAt = time.Now().UTC(), nil
				// SQL retries preserve StartedAt and do not emit another start.
				return putRecord(tx, "flow-step", key, step)
			}
			return nil
		}
		if !errors.Is(err, orchestration.ErrNotFound) {
			return err
		}
		now := time.Now().UTC()
		step = agentflow.Step{RunID: runID, ID: stepID, InputHash: inputHash, State: agentflow.StepRunning,
			Outputs: []agentflow.Value{}, Attempt: 1, StartedAt: now, UpdatedAt: now}
		if err := createRecord(tx, "flow-step", key, step); err != nil {
			return err
		}
		return flowAppendStepEvent(tx, run, orchestration.EventFlowStepStarted, stepID, map[string]any{"attempt": 1})
	})
	return
}

// FinishStep records the outputs used when replaying this step of the flow.
func (store *Store) FinishStep(ctx context.Context, runID, stepID string, outputs []agentflow.Value) (step agentflow.Step, err error) {
	encoded, err := json.Marshal(outputs)
	if err != nil {
		return step, err
	}
	err = store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		run, err := getRecord[agentflow.Run](tx, "flow", runID)
		if err != nil {
			return err
		}
		key := flowKey(runID, stepID)
		step, err = getRecord[agentflow.Step](tx, "flow-step", key)
		if err != nil {
			return err
		}
		if step.State == agentflow.StepCompleted {
			stored, err := json.Marshal(step.Outputs)
			if err != nil {
				return err
			}
			if !bytes.Equal(stored, encoded) {
				return fmt.Errorf("%w: completed step %s/%s", orchestration.ErrIdempotencyConflict, runID, stepID)
			}
			return nil
		}
		if step.State != agentflow.StepRunning {
			return fmt.Errorf("Flow step %s/%s is not running", runID, stepID)
		}
		now := time.Now().UTC()
		step.State, step.Outputs, step.Error = agentflow.StepCompleted, outputs, ""
		step.UpdatedAt, step.CompletedAt = now, &now
		if err := putRecord(tx, "flow-step", key, step); err != nil {
			return err
		}
		return flowAppendStepEvent(tx, run, orchestration.EventFlowStepCompleted, stepID, map[string]any{"output_count": len(outputs)})
	})
	return
}

// FailStep persists the failure reason for a flow step.
func (store *Store) FailStep(ctx context.Context, runID, stepID, reason string) error {
	return store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		run, err := getRecord[agentflow.Run](tx, "flow", runID)
		if err != nil {
			return err
		}
		key := flowKey(runID, stepID)
		step, err := getRecord[agentflow.Step](tx, "flow-step", key)
		if err != nil {
			return err
		}
		if step.State == agentflow.StepFailed && step.Error == reason {
			return nil
		}
		now := time.Now().UTC()
		step.State, step.Error, step.UpdatedAt, step.CompletedAt = agentflow.StepFailed, reason, now, &now
		if err := putRecord(tx, "flow-step", key, step); err != nil {
			return err
		}
		return flowAppendStepEvent(tx, run, orchestration.EventFlowStepFailed, stepID, map[string]any{"error": reason})
	})
}

// FinishRun persists the run's terminal state, its output values and the supplied reason.
func (store *Store) FinishRun(ctx context.Context, id string, state agentflow.State, outputs []agentflow.Value, reason string) (run agentflow.Run, err error) {
	if state != agentflow.StateCompleted && state != agentflow.StateFailed && state != agentflow.StateCanceled {
		return run, fmt.Errorf("unsupported terminal Flow state %q", state)
	}
	encoded, err := json.Marshal(outputs)
	if err != nil {
		return run, err
	}
	err = store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		run, err = getRecord[agentflow.Run](tx, "flow", id)
		if err != nil {
			return err
		}
		if run.State != agentflow.StateRunning {
			stored, err := json.Marshal(run.Outputs)
			if err != nil {
				return err
			}
			if run.State != state || run.Error != reason || !bytes.Equal(stored, encoded) {
				return fmt.Errorf("%w: terminal Flow %s", orchestration.ErrIdempotencyConflict, id)
			}
			return nil
		}
		now := time.Now().UTC()
		run.State, run.Outputs, run.Error, run.UpdatedAt, run.CompletedAt = state, outputs, reason, now, &now
		run.Version++
		if err := putRecord(tx, "flow", id, run); err != nil {
			return err
		}
		eventType := orchestration.EventFlowCompleted
		switch state {
		case agentflow.StateFailed:
			eventType = orchestration.EventFlowFailed
		case agentflow.StateCanceled:
			eventType = orchestration.EventFlowCanceled
		}
		_, err = appendEvents(tx, orchestration.Stream{Type: orchestration.StreamFlow, ID: id, TeamID: run.TeamID}, orchestration.AnySequence,
			orchestration.PendingEvent{Type: eventType, ActorAgentID: run.AgentID, CorrelationID: id,
				Payload: orchestration.MustJSONPayload(map[string]any{"output_count": len(outputs), "error": reason})})
		return err
	})
	return
}

// SessionAgent reads the agent bound to a named session in this flow run.
func (store *Store) SessionAgent(ctx context.Context, runID, name string) (id orchestration.AgentID, found bool, err error) {
	err = store.authority.View(ctx, func(tx *ateauthority.Tx) error {
		id, err = getRecord[orchestration.AgentID](tx, "flow-session", flowKey(runID, name))
		if errors.Is(err, orchestration.ErrNotFound) {
			return nil
		}
		found = err == nil
		return err
	})
	return
}

// BeginSessionStage saves the choice to spawn or continue so replay of the step follows
// the same path.
func (store *Store) BeginSessionStage(ctx context.Context, runID, name, stepID string) (stage agentflow.SessionStage, err error) {
	if name == "" || stepID == "" {
		return stage, errors.New("Flow Agent session stage requires session and step names")
	}
	err = store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		run, err := getRecord[agentflow.Run](tx, "flow", runID)
		if err != nil {
			return err
		}
		if run.State != agentflow.StateRunning {
			return fmt.Errorf("Flow %s is not running", runID)
		}
		key := flowKey(runID, name, stepID)
		stage, err = getRecord[agentflow.SessionStage](tx, "flow-stage", key)
		if err == nil {
			return nil
		}
		if !errors.Is(err, orchestration.ErrNotFound) {
			return err
		}
		stage = agentflow.SessionStage{RunID: runID, SessionName: name, StepID: stepID, Action: agentflow.SessionSpawn}
		_, err = getRecord[orchestration.AgentID](tx, "flow-session", flowKey(runID, name))
		if err == nil {
			stage.Action = agentflow.SessionContinue
		} else if !errors.Is(err, orchestration.ErrNotFound) {
			return err
		}
		return createRecord(tx, "flow-stage", key, stage)
	})
	return
}

// BindSessionAgent verifies that the agent belongs to the run's team and rejects rebinding
// the session to a different agent.
func (store *Store) BindSessionAgent(ctx context.Context, runID, name string, id orchestration.AgentID) error {
	if err := id.Validate(); err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("%w: Flow Agent session name is required", orchestration.ErrInvalidEvent)
	}
	return store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		run, err := getRecord[agentflow.Run](tx, "flow", runID)
		if err != nil {
			return err
		}
		agent, err := getRecord[orchestration.AgentSnapshot](tx, "agent", string(id))
		if err != nil {
			return err
		}
		if agent.TeamID != run.TeamID {
			return fmt.Errorf("%w: Agent %s does not belong to Team %s", orchestration.ErrInvalidEvent, id, run.TeamID)
		}
		key := flowKey(runID, name)
		stored, err := getRecord[orchestration.AgentID](tx, "flow-session", key)
		if err == nil {
			if stored != id {
				return fmt.Errorf("%w: Flow Agent session %s/%s", orchestration.ErrIdempotencyConflict, runID, name)
			}
			return nil
		}
		if !errors.Is(err, orchestration.ErrNotFound) {
			return err
		}
		return createRecord(tx, "flow-session", key, id)
	})
}

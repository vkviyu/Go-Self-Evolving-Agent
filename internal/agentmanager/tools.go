package agentmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

type spawnToolArgs struct {
	Prompt         string `json:"prompt"`
	Profile        string `json:"profile"`
	TaskID         string `json:"task_id"`
	ContextPolicy  string `json:"context_policy"`
	Workspace      string `json:"workspace"`
	IdempotencyKey string `json:"idempotency_key"`
}

func (supervisor *Supervisor) spawnTool(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	actor, err := actorFromContext(ctx)
	if err != nil {
		return nil, err
	}
	var args spawnToolArgs
	if err := decodeToolArgs(raw, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.Prompt) == "" {
		return nil, errors.New("prompt is required")
	}
	if args.IdempotencyKey == "" {
		args.IdempotencyKey = orchestration.ToolCallIDFromContext(ctx)
	}
	spec := orchestration.SpawnSpec{
		TeamID: actor.TeamID, ParentAgentID: actor.AgentID, ProfileName: args.Profile, InitialPrompt: args.Prompt,
		ContextPolicy: args.ContextPolicy, Workspace: orchestration.WorkspaceRequest(args.Workspace), TaskID: orchestration.TaskID(args.TaskID),
		IdempotencyKey: args.IdempotencyKey, CorrelationID: orchestration.ToolCallIDFromContext(ctx),
	}
	child, err := supervisor.Spawn(ctx, spec)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"agent_id": child.ID, "thread_id": child.ThreadID, "state": child.State, "version": child.Version, "workspace_mode": child.WorkspaceMode})
}

type listToolArgs struct {
	ParentAgentID string   `json:"parent_agent_id"`
	States        []string `json:"states"`
}

func (supervisor *Supervisor) listTool(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	actor, err := actorFromContext(ctx)
	if err != nil {
		return nil, err
	}
	var args listToolArgs
	if err := decodeToolArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.ParentAgentID != "" && args.ParentAgentID != string(actor.AgentID) {
		return nil, errors.New("agent_list can only inspect direct children of the current Agent")
	}
	query := orchestration.AgentQuery{TeamID: actor.TeamID, ParentAgentID: actor.AgentID}
	for _, state := range args.States {
		query.States = append(query.States, orchestration.AgentState(state))
	}
	agents, err := supervisor.ListAgents(ctx, query)
	if err != nil {
		return nil, err
	}
	return json.Marshal(agents)
}

type waitToolArgs struct {
	AgentIDs []string `json:"agent_ids"`
	Any      bool     `json:"any"`
	Timeout  int      `json:"timeout_ms"`
}

func (supervisor *Supervisor) waitTool(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	actor, err := actorFromContext(ctx)
	if err != nil {
		return nil, err
	}
	var args waitToolArgs
	if err := decodeToolArgs(raw, &args); err != nil {
		return nil, err
	}
	if len(args.AgentIDs) == 0 {
		return nil, ErrWaitInput
	}
	ids := make([]orchestration.AgentID, len(args.AgentIDs))
	for index, value := range args.AgentIDs {
		ids[index] = orchestration.AgentID(value)
		child, lookupErr := supervisor.agents.GetAgent(ctx, ids[index])
		if lookupErr != nil {
			return nil, lookupErr
		}
		if child.TeamID != actor.TeamID || child.ParentAgentID != actor.AgentID {
			return nil, errors.New("agent_wait can only wait for direct children of the current Agent")
		}
	}
	if actor.RunID != "" && supervisor.waitSets != nil {
		var deadline *time.Time
		if args.Timeout > 0 {
			value := time.Now().UTC().Add(time.Duration(args.Timeout) * time.Millisecond)
			deadline = &value
		}
		spec := orchestration.WaitSpec{
			TeamID: actor.TeamID, AgentID: actor.AgentID, RunID: actor.RunID, ToolCallID: orchestration.ToolCallIDFromContext(ctx),
			Condition: "children", TargetAgentIDs: ids, Any: args.Any, Deadline: deadline,
			ResumeToken: waitResumeToken(ctx), CorrelationID: orchestration.ToolCallIDFromContext(ctx), IdempotencyKey: orchestration.ToolCallIDFromContext(ctx),
		}
		if intent, resumed := orchestration.WaitIntentFromContext(ctx); resumed {
			// The first invocation only raises a checkpoint interrupt. On
			// resume, this idempotently loads the Supervisor-created WaitSet.
			spec = (&orchestration.WaitSuspendedError{WaitSet: intent}).WaitSpec()
			waitSet, suspendErr := supervisor.Suspend(ctx, spec)
			if suspendErr != nil {
				return nil, suspendErr
			}
			if waitSet.State == orchestration.WaitSetResolved || waitSet.State == orchestration.WaitSetExpired || waitSet.State == orchestration.WaitSetConsumed {
				agents, lookupErr := supervisor.lookupAgents(ctx, waitSet.TargetAgentIDs)
				if lookupErr != nil {
					return nil, lookupErr
				}
				return json.Marshal(WaitResult{Agents: agents, Satisfied: waitSet.State == orchestration.WaitSetResolved || waitSet.State == orchestration.WaitSetConsumed, TimedOut: waitSet.State == orchestration.WaitSetExpired})
			}
			return nil, &WaitSuspendedError{WaitSet: waitSet}
		}
		// Do not mutate Agent state before Eino has persisted its checkpoint.
		return nil, &WaitSuspendedError{WaitSet: orchestration.WaitSet{TeamID: spec.TeamID, AgentID: spec.AgentID, RunID: spec.RunID, ToolCallID: spec.ToolCallID, Condition: spec.Condition, TargetAgentIDs: spec.TargetAgentIDs, Any: spec.Any, Deadline: spec.Deadline, ResumeToken: spec.ResumeToken, CorrelationID: spec.CorrelationID, IdempotencyKey: spec.IdempotencyKey}}
	}
	result, err := supervisor.Wait(ctx, WaitCommand{AgentIDs: ids, Any: args.Any, Timeout: time.Duration(args.Timeout) * time.Millisecond})
	if err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

func waitResumeToken(ctx context.Context) string {
	if turnID := orchestration.TurnIDFromContext(ctx); turnID != "" {
		return turnID
	}
	return orchestration.ToolCallIDFromContext(ctx)
}

type interruptToolArgs struct {
	AgentID string `json:"agent_id"`
}

type continueToolArgs struct {
	AgentID        string `json:"agent_id"`
	Prompt         string `json:"prompt"`
	IdempotencyKey string `json:"idempotency_key"`
}

func (supervisor *Supervisor) continueTool(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	actor, err := actorFromContext(ctx)
	if err != nil {
		return nil, err
	}
	var args continueToolArgs
	if err := decodeToolArgs(raw, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.AgentID) == "" || strings.TrimSpace(args.Prompt) == "" {
		return nil, errors.New("agent_id and prompt are required")
	}
	child, err := supervisor.agents.GetAgent(ctx, orchestration.AgentID(args.AgentID))
	if err != nil {
		return nil, err
	}
	if child.TeamID != actor.TeamID || child.ParentAgentID != actor.AgentID {
		return nil, errors.New("agent_continue can only continue a direct child of the current Agent")
	}
	if args.IdempotencyKey == "" {
		args.IdempotencyKey = orchestration.ToolCallIDFromContext(ctx)
	}
	continued, err := supervisor.Continue(ctx, ContinueCommand{
		AgentID: child.ID, Prompt: args.Prompt, CorrelationID: orchestration.ToolCallIDFromContext(ctx), IdempotencyKey: args.IdempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"agent_id": continued.ID, "thread_id": continued.ThreadID, "state": continued.State, "version": continued.Version})
}

func (supervisor *Supervisor) resumeTool(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	actor, err := actorFromContext(ctx)
	if err != nil {
		return nil, err
	}
	var args struct {
		AgentID string `json:"agent_id"`
	}
	if err := decodeToolArgs(raw, &args); err != nil {
		return nil, err
	}
	child, err := supervisor.agents.GetAgent(ctx, orchestration.AgentID(args.AgentID))
	if err != nil {
		return nil, err
	}
	if child.TeamID != actor.TeamID || child.ParentAgentID != actor.AgentID {
		return nil, errors.New("agent_resume can only resume a direct child")
	}
	resumed, err := supervisor.ResumeAgent(ctx, child.ID)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"agent_id": resumed.ID, "state": resumed.State, "run_id": resumed.CurrentRunID, "activity": resumed.Activity})
}

func (supervisor *Supervisor) interruptTool(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	actor, err := actorFromContext(ctx)
	if err != nil {
		return nil, err
	}
	var args interruptToolArgs
	if err := decodeToolArgs(raw, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.AgentID) == "" {
		return nil, errors.New("agent_id is required")
	}
	child, err := supervisor.agents.GetAgent(ctx, orchestration.AgentID(args.AgentID))
	if err != nil {
		return nil, err
	}
	if child.TeamID != actor.TeamID || child.ParentAgentID != actor.AgentID {
		return nil, errors.New("agent_interrupt can only interrupt direct children of the current Agent")
	}
	if err := supervisor.Interrupt(ctx, orchestration.AgentID(args.AgentID)); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"agent_id": args.AgentID, "state": orchestration.AgentCanceling})
}

type messageToolArgs struct {
	RecipientAgentID string          `json:"recipient_agent_id"`
	Type             string          `json:"type"`
	Body             json.RawMessage `json:"body"`
	IdempotencyKey   string          `json:"idempotency_key"`
}

func (supervisor *Supervisor) messageTool(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	actor, err := actorFromContext(ctx)
	if err != nil {
		return nil, err
	}
	var args messageToolArgs
	if err := decodeToolArgs(raw, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.RecipientAgentID) == "" {
		return nil, errors.New("recipient_agent_id is required")
	}
	recipient, err := supervisor.agents.GetAgent(ctx, orchestration.AgentID(args.RecipientAgentID))
	if err != nil {
		return nil, err
	}
	if recipient.TeamID != actor.TeamID || (recipient.ParentAgentID != actor.AgentID && actor.ParentAgentID != recipient.ID) {
		return nil, errors.New("agent_message only supports parent-child communication")
	}
	if len(args.Body) == 0 || !json.Valid(args.Body) {
		return nil, errors.New("body must be valid JSON")
	}
	if args.IdempotencyKey == "" {
		args.IdempotencyKey = orchestration.ToolCallIDFromContext(ctx)
	}
	message, err := supervisor.Send(ctx, orchestration.MessageSpec{
		TeamID: actor.TeamID, SenderAgentID: actor.AgentID, RecipientAgentID: recipient.ID,
		Type: orchestration.MessageType(args.Type), Body: args.Body,
		CorrelationID: orchestration.ToolCallIDFromContext(ctx), IdempotencyKey: args.IdempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"message_id": message.ID, "recipient_agent_id": message.RecipientAgentID, "sequence": message.Sequence, "delivery_state": message.DeliveryState})
}

// actorFromContext requires a nonempty, valid actor scope before control tools can act on
// a team's behalf.
func actorFromContext(ctx context.Context) (orchestration.ActorScope, error) {
	actor, ok := orchestration.ActorScopeFromContext(ctx)
	if !ok || actor.IsZero() {
		return orchestration.ActorScope{}, errors.New("Agent control tool requires an actor scope")
	}
	if err := actor.Validate(); err != nil {
		return orchestration.ActorScope{}, fmt.Errorf("invalid actor scope: %w", err)
	}
	return actor, nil
}

// decodeToolArgs decodes agent control arguments and rejects unknown struct fields.
func decodeToolArgs(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid Agent control arguments: %w", err)
	}
	return nil
}

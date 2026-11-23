package atebridge

import (
	"context"
	"encoding/json"

	"github.com/vkviyu/agent-temporal-engine/agentprofile"
	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/memory"
	"github.com/vkviyu/agent-temporal-engine/model"
)

func (b *Bridge) completeSuccess(ctx context.Context, c *client.Client, key model.KnowledgeKey, task agentprofile.Task, result memory.Source) error {
	base := string(key.Execution)
	binding := b.authority()
	if err := b.finishStep(ctx, c, key); err != nil {
		return err
	}

	// This policy attests only the supplied durable host outcome. It neither
	// runs a checker nor claims the source's tools or business goal were verified.
	command, err := b.putValue(ctx, c, base+"/validation-command", []byte(
		"gsea.durable-terminal/v1: accept the insert-once host terminal record only when Succeeded is true; record-only execution history, no independent goal or tool validation"))
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(struct{ Schema, Namespace, Principal string }{"gsea.record-only-environment/v1", b.namespace, "gsea"})
	environment, err := b.putValue(ctx, c, base+"/validation-environment", raw)
	if err != nil {
		return err
	}
	goal, err := agentprofile.GoalKnowledgeDigest(task)
	if err != nil {
		return err
	}
	required := agentprofile.ValidationRequirement{
		Version: 1, Source: environment.Anchor.Fence,
		Spec: agentprofile.ValidationSpec{
			Space: b.OperationalSpace(), Task: task.ID, Execution: key.Execution,
			GoalEpoch: 1, Goal: goal, Result: result.SourceID, Command: command.SourceID,
			Environment: environment.SourceID, ToolVersion: "gsea-terminal-recorder/v1",
			PolicyVersion: "durable-host-outcome-attestation/v1",
		},
	}
	required.ID = agentprofile.ValidationRequirementID(required)
	if _, err := b.agent(ctx, c, base+"/bind-validation", agentprofile.Command{Control: &agentprofile.ControlCommand{
		BindValidation: &agentprofile.BindValidationCommand{Requirement: required, Authority: binding},
	}}); err != nil {
		return err
	}
	envelope := agentprofile.ValidationEnvelope{
		Requirement: required.ID, Spec: required.Spec, Passed: true, Evidence: []model.ObjectID{result.SourceID},
	}
	raw, err = envelope.Canonical()
	if err != nil {
		return err
	}
	proof, err := b.putValue(ctx, c, base+"/validation-envelope", raw)
	if err != nil {
		return err
	}
	_, err = b.agent(ctx, c, base+"/complete", agentprofile.Command{Control: &agentprofile.ControlCommand{
		CompleteTask: &agentprofile.CompleteTaskCommand{
			Task: task.ID, Execution: key.Execution, Epoch: 1,
			Result: result.SourceID, Validation: proof.SourceID, Authority: binding,
		},
	}})
	return err
}

// Keep the exact public mutation contract visible to downstream integrations.
var _ interface {
	WithClient(context.Context, func(*client.Client) error) error
	Execute(context.Context, string, func(*client.Client) (api.CommitRequest, error)) (api.CommitResult, error)
	ExecuteInSpace(context.Context, model.SpaceID, string, func(*client.Client) (api.CommitRequest, error)) (api.CommitResult, error)
	EnsureExecution(context.Context, string, string, string) (model.KnowledgeKey, error)
	HasExecution(context.Context, string, string) (bool, error)
	CompleteExecution(context.Context, string, string, bool) error
	OperationalSpace() model.SpaceID
	RecoverPending(context.Context) error
} = (*Bridge)(nil)

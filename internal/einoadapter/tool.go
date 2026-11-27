// Package einoadapter exposes the application's stable tool boundary to Eino.
//
// Eino owns model/tool-call orchestration. This package deliberately keeps
// policy, approval, revision selection and durability in the application
// pipeline so every execution path has the same security semantics.
package einoadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	appTool "github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

// Tool is an Eino adapter bound to the registry snapshot captured for its turn.
type Tool struct {
	descriptor        appTool.Descriptor
	pipeline          *appTool.Pipeline
	registry          *appTool.Snapshot
	sessionID         session.ID
	turnID            string
	runtimeSnapshotID string
	actor             orchestration.ActorScope
	execution         orchestration.ExecutionBinding
}

var _ einotool.InvokableTool = (*Tool)(nil)

// NewToolsWithExecution attaches both actor identity and the immutable
// execution scope to every Eino tool invocation.
func NewToolsWithExecution(
	pipeline *appTool.Pipeline,
	registry *appTool.Snapshot,
	sessionID session.ID,
	runtimeSnapshotID string,
	turnID string,
	actor orchestration.ActorScope,
	execution orchestration.ExecutionBinding,
) ([]einotool.BaseTool, error) {
	if pipeline == nil || registry == nil {
		return nil, errors.New("Eino tools require a pipeline and registry snapshot")
	}
	if sessionID == "" {
		return nil, errors.New("Eino tools require a session ID")
	}
	if turnID == "" {
		return nil, errors.New("Eino tools require a turn ID")
	}
	if !actor.IsZero() {
		if err := actor.Validate(); err != nil {
			return nil, fmt.Errorf("Eino tools require a valid actor scope: %w", err)
		}
	}
	if err := execution.Validate(); err != nil {
		return nil, fmt.Errorf("Eino tools require a valid execution binding: %w", err)
	}
	descriptors := registry.Descriptors()
	tools := make([]einotool.BaseTool, 0, len(descriptors))
	for _, descriptor := range descriptors {
		tools = append(tools, &Tool{
			descriptor:        descriptor,
			pipeline:          pipeline,
			registry:          registry,
			sessionID:         sessionID,
			turnID:            turnID,
			runtimeSnapshotID: runtimeSnapshotID,
			actor:             actor,
			execution:         execution,
		})
	}
	return tools, nil
}

// Info converts the registry's canonical JSON Schema into Eino metadata.
func (t *Tool) Info(context.Context) (*schema.ToolInfo, error) {
	var input jsonschema.Schema
	if err := json.Unmarshal(t.descriptor.InputSchema, &input); err != nil {
		return nil, fmt.Errorf("decode input schema for %s@%s: %w", t.descriptor.Name, t.descriptor.Revision, err)
	}
	return &schema.ToolInfo{
		Name: t.descriptor.Name,
		Desc: t.descriptor.Description,
		Extra: map[string]any{
			"revision":   t.descriptor.Revision,
			"risk_class": t.descriptor.RiskClass,
		},
		ParamsOneOf: schema.NewParamsOneOfByJSONSchema(&input),
	}, nil
}

// InvokableRun routes Eino calls through the central execution pipeline.
func (t *Tool) InvokableRun(ctx context.Context, argumentsInJSON string, _ ...einotool.Option) (string, error) {
	callID := compose.GetToolCallID(ctx)
	if callID == "" {
		return "", errors.New("Eino tool invocation requires a durable model tool-call ID")
	}
	ctx = orchestration.WithActorScope(ctx, t.actor)
	result, err := t.pipeline.Execute(ctx, appTool.Call{
		Actor:             t.actor,
		Execution:         t.execution,
		SessionID:         t.sessionID,
		TurnID:            t.turnID,
		RuntimeSnapshotID: t.runtimeSnapshotID,
		CallID:            callID,
		ToolName:          t.descriptor.Name,
		Arguments:         json.RawMessage(argumentsInJSON),
		Registry:          t.registry,
	})
	if err != nil {
		if onlyDenial(err, appTool.ErrApprovalDenied) {
			return deniedToolResult(), nil
		}
		if onlyDenial(err, appTool.ErrPolicyDenied) {
			return `{"error":"tool call denied by policy","message":"The host policy does not allow this action. Choose an allowed tool or explain the restriction; do not attempt to bypass it."}`, nil
		}
		if precondition, ok := errors.AsType[*appTool.PreconditionError](err); ok {
			encoded, encodeErr := json.Marshal(map[string]string{
				"error": precondition.Code, "message": precondition.Message,
			})
			return string(encoded), encodeErr
		}
		if executionErr, ok := errors.AsType[*appTool.ExecutionError](err); ok {
			encoded, encodeErr := json.Marshal(map[string]string{
				"error":   executionErr.Error(),
				"message": "The tool failed. Correct the request or choose another approach.",
			})
			if encodeErr != nil {
				return "", encodeErr
			}
			return string(encoded), nil
		}
		if suspended, ok := errors.AsType[*orchestration.WaitSuspendedError](err); ok {
			encoded, encodeErr := orchestration.EncodeWaitInterrupt(suspended.WaitSet)
			if encodeErr != nil {
				return "", encodeErr
			}
			// Eino persists its runner state before exposing this control
			// event to the Supervisor. The Supervisor creates the durable
			// WaitSet only after the checkpoint exists.
			return "", einotool.StatefulInterrupt(ctx, encoded, encoded)
		}
		return "", err
	}
	return string(result.Data), nil
}

// A joined durability error must remain fatal even when its sibling is a
// denial. errors.Is alone would hide a failed audit write in that case.
func onlyDenial(err, target error) bool {
	if err == target {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !onlyDenial(child, target) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return onlyDenial(wrapped.Unwrap(), target)
	}
	return false
}

func deniedToolResult() string {
	return `{"error":"tool call approval denied","message":"The user denied permission to run this tool."}`
}

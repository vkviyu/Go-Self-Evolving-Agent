package atebridge

import (
	"context"
	"errors"

	"github.com/vkviyu/agent-temporal-engine/agentprofile"
	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/model"
)

// ATE requires FinishStep and its continuation in one native Work publication.
// The reserved next action records only this closure, then is reconciled before
// CompleteTask. It has no tool/model/external effect.
func (b *Bridge) finishStep(ctx context.Context, c *client.Client, key model.KnowledgeKey) error {
	base := string(key.Execution)
	nextKey := "execution/" + base + "/step-closure"
	actionKey := "gsea-closure-" + digest(b.namespace, base)
	body, err := api.EncodeStateBatch(api.StateBatch{Operations: []api.StateMutation{{
		Key: []byte("gsea/step-closure/v1/" + base), Kind: api.MutationPut,
		Value:     api.Value{Kind: api.ValueBytes, Data: []byte("record-only turn step closed")},
		Condition: api.ValueCondition{Kind: api.ConditionAbsent},
	}}})
	if err != nil {
		return err
	}
	next := api.CommitRequest{
		Space: b.OperationalSpace(), Principal: "gsea",
		RequestID: b.requestID(nextKey),
		Method:    api.MethodMutateState, Body: body,
	}
	// The native checkpoint reserves the exact next action; aterequest owns submission.
	binding := b.authority()
	_, err = b.execute(ctx, c, "execution/"+base+"/finish-step", func(c *client.Client) (api.CommitRequest, error) {
		known, err := c.GetKnowledgeState(ctx, api.ReadKnowledgeStateRequest{
			Space: b.OperationalSpace(), Principal: "gsea", Knowledge: &key,
		})
		if err != nil {
			return api.CommitRequest{}, err
		}
		if known.Revision == nil {
			return api.CommitRequest{}, errors.New("atebridge: execution knowledge revision missing")
		}
		intent := agentprofile.ActionIntent{
			Action: agentprofile.PendingAction{
				Key: actionKey, Owner: key.Execution, Generation: 1, Status: agentprofile.ActionPending,
				Request: next.Key(), InputFence: known.Fence,
				CommandDigest: agentprofile.ExactRequestDigest(next.Body),
			},
			Method: next.Method, ExactCommand: next.Body, Authority: binding,
		}
		continuation := agentprofile.Continuation{
			Execution: key.Execution, Version: 1, Incarnation: 1,
			PendingKey: actionKey, PendingGeneration: 1, InputFence: known.Fence,
			Phase: "record-terminal-outcome",
			Position: &agentprofile.ContinuationPosition{
				Space: b.OperationalSpace(), Principal: "gsea", Step: 1,
				KnowledgeFence: known.Fence, KnowledgeKey: key,
				KnowledgeCursor: model.Cursor{Store: known.Fence.Store, Space: b.OperationalSpace(), Revision: known.Revision.ID},
				AdapterVersion:  "gsea-record-only/v1",
				NextActions:     []agentprofile.PendingActionRef{{Key: actionKey, Generation: 1}},
			},
		}
		// Pin the actual published revision, including its empty genesis when
		// nothing has been adopted. Never invent an observation to close a step.
		finish, err := api.AgentWorkStep(agentprofile.Command{Control: &agentprofile.ControlCommand{
			FinishStep: &agentprofile.FinishStepCommand{Execution: key.Execution, Sequence: 1, Epoch: 1, Authority: binding},
		}})
		if err != nil {
			return api.CommitRequest{}, err
		}
		checkpoint, err := api.AgentWorkStep(agentprofile.Command{Control: &agentprofile.ControlCommand{
			Checkpoint: &agentprofile.CheckpointCommand{Intent: intent, Continuation: continuation, Authority: binding},
		}})
		if err != nil {
			return api.CommitRequest{}, err
		}
		raw, err := api.EncodeWorkUnit(api.WorkUnit{Kind: "checkpoint", Steps: []api.WorkStep{finish, checkpoint}})
		return api.CommitRequest{Space: b.OperationalSpace(), Method: api.MethodApplyWork, Body: raw}, err
	})
	if err != nil {
		return err
	}
	receipt, err := b.execute(ctx, c, nextKey, func(*client.Client) (api.CommitRequest, error) { return next, nil })
	if err != nil {
		return err
	}
	_, err = b.agent(ctx, c, base+"/resolve-closure", agentprofile.Command{Control: &agentprofile.ControlCommand{
		ResolveAction: &agentprofile.ResolveActionCommand{Resolution: agentprofile.ActionResolution{
			Key: actionKey, Generation: 1, Outcome: "applied", ObservedFence: receipt.Fence, Receipt: receipt.ResultDigest,
		}},
	}})
	return err
}

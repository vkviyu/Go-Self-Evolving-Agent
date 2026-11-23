package experience

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/vkviyu/agent-temporal-engine/agentprofile"
	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/knowledge"
	"github.com/vkviyu/agent-temporal-engine/memory"
	"github.com/vkviyu/agent-temporal-engine/model"
)

const memoryAdoptionLabel = "GSEA explicit model-selected memory; retired record-only call scope"

// toolAdopt never borrows a model-supplied execution ID or the automatic
// Prepare context. The record-only scope is deterministically bound to trusted
// session/turn/actor identity. It neither dispatches nor validates any work.
func (s *Service) toolAdopt(ctx context.Context, key string, caller MemoryCaller, ref string) (out json.RawMessage, err error) {
	scope := key + "/explicit-memory/" + caller.ScopeDigest
	id := digest(scope)
	target := model.KnowledgeKey{
		Actor: model.ActorID(caller.ActorID), Execution: model.ExecutionID("gsea-memory-call-" + id),
	}
	task := agentprofile.Task{
		ID: model.EntityID("gsea-memory-task-" + id), Space: s.bridge.OperationalSpace(), Owner: "gsea",
		Goal:      "Record-only explicit memory selection for a trusted GSEA call; retire without attesting user work.",
		GoalEpoch: 1, Status: agentprofile.TaskOpen,
	}
	defer func() {
		// Retirement does not depend on current source eligibility. A revoked
		// source must not strand an allocation created before a lost response.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		err = errors.Join(err, s.retireMemoryScope(cleanup, scope, task.ID))
		if err != nil {
			out = nil
		}
	}()
	var selected MemoryToolRecord
	if found, err := s.load(ctx, key+"/selected", &selected); err != nil {
		return nil, err
	} else if !found {
		selected, err = s.toolRead(ctx, ref)
		if err != nil {
			return nil, err
		}
		if !selected.Reusable {
			return nil, ErrSelection
		}
		if err := s.save(ctx, key+"/selected", selected); err != nil {
			return nil, err
		}
	}
	// A recorded acknowledgement is not permission for new use. Recheck
	// current reuse policy and the frozen source anchor on every invocation.
	check := Context{
		SessionID: caller.SessionID,
		Memories:  []Selection{{Source: selected.Source}},
		Sources:   []memory.Source{selected.Source},
	}
	if err := s.checkSources(ctx, check); err != nil {
		return nil, err
	}
	// Persist allocation intent before submission so retirement can reconcile
	// the exact task command without scanning the global control catalog.
	if err := s.save(ctx, scope+"/allocation", task); err != nil {
		return nil, err
	}
	commands := []agentprofile.Command{
		{CreateTask: &task},
		{Spawn: &agentprofile.Execution{
			ID: target.Execution, Task: task.ID, Actor: target.Actor,
			Incarnation: 1, AcceptedGoalEpoch: 1, Status: agentprofile.ExecutionReady,
		}},
	}
	for i, command := range commands {
		body, err := agentprofile.EncodeCommand(command)
		if err != nil {
			return nil, err
		}
		suffix := "/task"
		if i == 1 {
			suffix = "/execution"
		}
		if _, err := s.commit(ctx, scope+suffix, api.CommitRequest{
			Space: s.bridge.OperationalSpace(), Method: api.MethodApplyAgent, Body: body,
		}); err != nil {
			return nil, err
		}
	}
	var adopt knowledge.AdoptMemoryCommand
	if found, err := s.load(ctx, key+"/adoption", &adopt); err != nil {
		return nil, err
	} else if !found {
		adopt = knowledge.AdoptMemoryCommand{
			Target: target, Source: knowledge.SourceAnchor{Anchor: selected.Source.Anchor},
			Memory: selected.Source.SourceID, Label: memoryAdoptionLabel,
		}
		err = s.bridge.WithClient(ctx, func(c *client.Client) error {
			state, err := c.GetKnowledgeState(ctx, api.ReadKnowledgeStateRequest{Space: s.bridge.OperationalSpace(), Knowledge: &target})
			if err != nil {
				return err
			}
			if state.Revision != nil {
				adopt.ExpectedSequence = state.Revision.Sequence
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if err := adopt.Validate(); err != nil {
			return nil, err
		}
		// Sequence conflicts remain conflicts; never rebuild a pending request.
		if err := s.save(ctx, key+"/adoption", adopt); err != nil {
			return nil, err
		}
	}
	body, err := knowledge.EncodeCommand(knowledge.Command{AdoptMemory: &adopt})
	if err != nil {
		return nil, err
	}
	if _, err := s.commit(ctx, key+"/adopt", api.CommitRequest{
		Space: s.bridge.OperationalSpace(), Method: api.MethodApplyKnowledge, Body: body,
	}); err != nil {
		return nil, err
	}
	observation := knowledge.Observation{
		Key: adopt.Target, OwnerSpace: s.bridge.OperationalSpace(), Source: adopt.Source,
		Payload: adopt.Memory, PayloadKind: "memory", Sequence: adopt.ExpectedSequence + 1, Label: adopt.Label,
	}
	return json.Marshal(memoryManageResult{
		Operation: "adopt", Status: "adopted", Ref: selected.Ref, Claim: selected.Claim,
		Adopted: true, Execution: &adopt.Target, Observation: knowledge.ObservationID(observation),
		Note: "Explicit adoption recorded; its record-only call scope is retired without attesting execution of user work. Current reuse was checked for this call; this is not continuous authorization.",
	})
}

// retireMemoryScope closes a metadata-only allocation with native cancellation,
// preserving knowledge observations without inventing a successful work step.
func (s *Service) retireMemoryScope(ctx context.Context, key string, taskID model.EntityID) error {
	var task agentprofile.Task
	if found, err := s.load(ctx, key+"/allocation", &task); err != nil {
		return err
	} else if !found {
		return nil
	}
	if task.ID != taskID {
		return ErrSelection
	}
	body, err := agentprofile.EncodeCommand(agentprofile.Command{CreateTask: &task})
	if err != nil {
		return err
	}
	if _, err := s.commit(ctx, key+"/task", api.CommitRequest{
		Space: s.bridge.OperationalSpace(), Method: api.MethodApplyAgent, Body: body,
	}); err != nil {
		return err
	}
	body, err = agentprofile.EncodeCommand(agentprofile.Command{CancelTask: &agentprofile.CancelTaskCommand{Task: taskID}})
	if err != nil {
		return err
	}
	_, err = s.commit(ctx, key+"/retire", api.CommitRequest{
		Space: s.bridge.OperationalSpace(), Method: api.MethodApplyAgent, Body: body,
	})
	return err
}

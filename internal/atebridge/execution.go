package atebridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/vkviyu/agent-temporal-engine/agentprofile"
	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/agent-temporal-engine/spacecatalog"
)

// MaxGoalBytes bounds the complete UTF-8 goal, not its rune count. It leaves
// room for the JSON string framing of the bounded native state value.
const MaxGoalBytes = api.MaxStateValueBytes - 64

// EnsureExecution creates one record-only task/execution/step per turn. The
// returned key is native execution knowledge scope for later explicit adoption;
// creation itself adds no observations and dispatches no external work.
// The full goal (up to MaxGoalBytes) is published before the task. Goals larger
// than ATE's task text limit use an explicit immutable reference in Task.Goal.
func (b *Bridge) EnsureExecution(ctx context.Context, sessionID, turnID, goal string) (model.KnowledgeKey, error) {
	if ctx == nil || sessionID == "" || turnID == "" {
		return model.KnowledgeKey{}, errors.New("atebridge: session and turn required")
	}
	if goal == "" || len(goal) > MaxGoalBytes || !utf8.ValidString(goal) {
		return model.KnowledgeKey{}, errors.New("atebridge: goal must be nonempty UTF-8 and at most 1 MiB")
	}
	key, originalTask := b.execution(sessionID, turnID, goal)
	// State may acquire the same process lease; pin before entering it.
	if err := b.pinGoal(ctx, key, goal); err != nil {
		return model.KnowledgeKey{}, err
	}
	err := b.withMutation(ctx, func(c *client.Client) error {
		if err := b.ensureSpace(ctx, c); err != nil {
			return err
		}
		task, err := b.publishGoal(ctx, c, key, originalTask)
		if err != nil {
			return err
		}
		commands := []agentprofile.Command{
			{CreateTask: &task},
			{Spawn: &agentprofile.Execution{
				ID: key.Execution, Task: task.ID, Actor: key.Actor,
				Incarnation: 1, AcceptedGoalEpoch: 1, Status: agentprofile.ExecutionReady,
			}},
		}
		for i, cmd := range commands {
			name := "task"
			if i == 1 {
				name = "spawn"
			}
			if _, err := b.agent(ctx, c, string(key.Execution)+"/"+name, cmd); err != nil {
				return err
			}
		}
		if _, err := b.execute(ctx, c, "execution/"+string(key.Execution)+"/step", func(c *client.Client) (api.CommitRequest, error) {
			snapshot, err := c.Snapshot(ctx)
			if err != nil {
				return api.CommitRequest{}, err
			}
			return b.agentRequest(agentprofile.Command{StartStep: &agentprofile.Step{
				Execution: key.Execution, Sequence: 1, GoalEpoch: 1,
				Status: agentprofile.StepOpen, InputAnchor: snapshot.Fence,
			}})
		}); err != nil {
			return err
		}
		_, err = b.agent(ctx, c, string(key.Execution)+"/authority", agentprofile.Command{Control: &agentprofile.ControlCommand{
			GrantAuthority: &agentprofile.GrantAuthorityCommand{Authority: agentprofile.ExecutionAuthority{
				Execution: key.Execution, Binding: b.authority(), Status: "active",
				Evidence: model.ObjectID(digest("gsea record-only history authority/v1", b.namespace, string(key.Execution))),
			}},
		}})
		return err
	})
	if err != nil {
		return model.KnowledgeKey{}, err
	}
	return key, nil
}

// CompleteExecution records a caller-attested durable terminal outcome. Success
// uses native FinishStep/BindValidation/CompleteTask; failure uses CancelTask
// (ATE has no failed execution status) plus an immutable succeeded=false record.
// Conflicting outcomes for the same turn fail rather than rewrite history.
func (b *Bridge) CompleteExecution(ctx context.Context, sessionID, turnID string, succeeded bool) error {
	ensured, err := b.HasExecution(ctx, sessionID, turnID)
	if err != nil {
		return err
	}
	if !ensured {
		return errors.New("atebridge: EnsureExecution must finish before completion")
	}
	key, _ := b.execution(sessionID, turnID, "")
	terminal, _ := json.Marshal(struct {
		Schema, Namespace, SessionID, TurnID string
		Succeeded                            bool
	}{"gsea.durable-terminal/v1", b.namespace, sessionID, turnID, succeeded})
	// State prerequisites must finish before acquiring the native mutation lease.
	if err := b.state.Put(ctx, b.localKey("terminal", string(key.Execution)), terminal); err != nil {
		return err
	}
	return b.withMutation(ctx, func(c *client.Client) error {
		task, err := b.originalTask(ctx, c, key)
		if err != nil {
			return err
		}
		result, err := b.putValue(ctx, c, "terminal/"+string(key.Execution), terminal)
		if err != nil {
			return err
		}
		if !succeeded {
			_, err = b.agent(ctx, c, string(key.Execution)+"/cancel", agentprofile.Command{CancelTask: &agentprofile.CancelTaskCommand{Task: task.ID}})
			return err
		}
		return b.completeSuccess(ctx, c, key, task, result)
	})
}

// pinGoal pins the original text's digest before its first publication.
// The complete bytes live in the native execution value, not a state copy.
func (b *Bridge) pinGoal(ctx context.Context, key model.KnowledgeKey, goal string) error {
	raw, _ := json.Marshal(digest(goal))
	if err := b.state.Put(ctx, b.localKey("goal", string(key.Execution)), raw); err != nil {
		return fmt.Errorf("atebridge: pin original goal: %w", err)
	}
	return nil
}

func (b *Bridge) publishGoal(ctx context.Context, c *client.Client, key model.KnowledgeKey, task agentprofile.Task) (agentprofile.Task, error) {
	source, err := b.putValue(ctx, c, "goal/"+string(key.Execution), []byte(task.Goal))
	if err != nil {
		return agentprofile.Task{}, err
	}
	if len(task.Goal) > 1024 {
		anchor, err := json.Marshal(source.Anchor)
		if err != nil {
			return agentprofile.Task{}, err
		}
		task.Goal = fmt.Sprintf("GSEA turn %s; full goal ate.value digest %s; anchor %s", key.Execution, source.SourceID, anchor)
	}
	if err := (agentprofile.Command{CreateTask: &task}).Validate(); err != nil {
		return agentprofile.Task{}, err
	}
	return task, nil
}

// HasExecution reports whether the task, execution, step and authority were
// fully prepared. It reconciles the final command's native receipt even after a
// lost response, without creating native records or retrying a pending mutation.
// Completed/canceled executions still exist and return true.
func (b *Bridge) HasExecution(ctx context.Context, sessionID, turnID string) (bool, error) {
	if ctx == nil || sessionID == "" || turnID == "" {
		return false, errors.New("atebridge: session and turn required")
	}
	key, _ := b.execution(sessionID, turnID, "")
	results := make(chan bool, 1)
	err := b.withMutation(ctx, func(c *client.Client) error {
		found, err := b.hasExecution(ctx, c, key)
		if err == nil {
			results <- found
		}
		return err
	})
	if err != nil {
		return false, err
	}
	select {
	case found := <-results:
		return found, nil
	default:
		return false, errors.New("atebridge: process returned without checking preparation")
	}
}

func (b *Bridge) hasExecution(ctx context.Context, c *client.Client, key model.KnowledgeKey) (bool, error) {
	lookup, err := b.lookup(ctx, c, "execution/"+string(key.Execution)+"/authority")
	return lookup.Code == api.CodeDurableAck, err
}

// lookup reads canonical control metadata through the existing client lease.
func (b *Bridge) lookup(ctx context.Context, c *client.Client, operation string) (api.LookupRequestResult, error) {
	key := api.RequestKey{Space: b.OperationalSpace(), Principal: "gsea", RequestID: b.requestID(operation)}
	snapshot, err := c.Snapshot(ctx)
	if err != nil {
		return api.LookupRequestResult{}, err
	}
	lookup, err := c.LookupRequest(ctx, api.LookupRequestRequest{Space: key.Space, Principal: key.Principal, RequestID: key.RequestID})
	if err != nil {
		return lookup, err
	}
	return lookup, verifyNativeLookup(key, snapshot.Fence.Store, lookup)
}

func (b *Bridge) originalTask(ctx context.Context, c *client.Client, key model.KnowledgeKey) (agentprofile.Task, error) {
	receipt, err := b.lookup(ctx, c, "execution/"+string(key.Execution)+"/task")
	if err != nil {
		return agentprofile.Task{}, err
	}
	if receipt.Code != api.CodeDurableAck {
		return agentprofile.Task{}, errors.New("atebridge: original task publication missing")
	}
	anchor := model.SnapshotAnchor{Space: b.OperationalSpace(), Fence: receipt.Result.Fence}
	state, err := c.ReadFullAgentState(ctx, api.ReadAgentStateRequest{Space: b.OperationalSpace(), Anchor: &anchor})
	if err != nil {
		return agentprofile.Task{}, err
	}
	id := model.EntityID("gsea-task-" + strings.TrimPrefix(string(key.Execution), "gsea-turn-"))
	for _, task := range state.Snapshot.Tasks {
		if task.ID == id {
			return task, nil
		}
	}
	return agentprofile.Task{}, errors.New("atebridge: canonical task missing at original publication")
}

func (b *Bridge) execution(sessionID, turnID, goal string) (model.KnowledgeKey, agentprofile.Task) {
	id := digest(b.namespace, sessionID, turnID)
	key := model.KnowledgeKey{Actor: "gsea", Execution: model.ExecutionID("gsea-turn-" + id)}
	task := agentprofile.Task{ID: model.EntityID("gsea-task-" + id), Space: b.OperationalSpace(), Owner: "gsea", Goal: goal, GoalEpoch: 1, Status: agentprofile.TaskOpen}
	return key, task
}

func (b *Bridge) authority() agentprofile.AuthorityBinding {
	return agentprofile.AuthorityBinding{Deployment: "gsea-history-" + b.namespace, OwnerEpoch: 1, Incarnation: 1, Holder: "gsea"}
}

func (b *Bridge) agentRequest(command agentprofile.Command) (api.CommitRequest, error) {
	body, err := agentprofile.EncodeCommand(command)
	return api.CommitRequest{Space: b.OperationalSpace(), Method: api.MethodApplyAgent, Body: body}, err
}

func (b *Bridge) agent(ctx context.Context, c *client.Client, key string, command agentprofile.Command) (api.CommitResult, error) {
	return b.execute(ctx, c, "execution/"+key, func(*client.Client) (api.CommitRequest, error) {
		return b.agentRequest(command)
	})
}

func (b *Bridge) ensureSpace(ctx context.Context, c *client.Client) error {
	_, err := b.execute(ctx, c, "operational-space", func(*client.Client) (api.CommitRequest, error) {
		body, err := spacecatalog.EncodeApply(spacecatalog.Apply{
			Name:        "Execution history",
			Description: "GSEA execution goals and record-only turn outcomes. Original session events remain in the native session authority. Nonsemantic execution evidence; not curated knowledge or independently verified tool success.",
		})
		return api.CommitRequest{Space: b.OperationalSpace(), Method: api.MethodApplySpace, Body: body}, err
	})
	return err
}

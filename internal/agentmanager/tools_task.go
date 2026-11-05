package agentmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

type taskCreateToolArgs struct {
	Title               string `json:"title"`
	Description         string `json:"description"`
	ParentTaskID        string `json:"parent_task_id"`
	Priority            int    `json:"priority"`
	RequiredProfile     string `json:"required_profile"`
	VerificationProfile string `json:"verification_profile"`
	IdempotencyKey      string `json:"idempotency_key"`
}

type taskRefineToolArgs struct {
	ParentTaskID    string `json:"parent_task_id"`
	ExpectedVersion uint64 `json:"expected_version"`
	IdempotencyKey  string `json:"idempotency_key"`
	Children        []struct {
		Key                 string `json:"key"`
		Title               string `json:"title"`
		Description         string `json:"description"`
		Priority            int    `json:"priority"`
		RequiredProfile     string `json:"required_profile"`
		VerificationProfile string `json:"verification_profile"`
	} `json:"children"`
	Dependencies []struct {
		ChildKey           string `json:"child_key"`
		DependencyChildKey string `json:"dependency_child_key"`
		DependencyTaskID   string `json:"dependency_task_id"`
	} `json:"dependencies"`
}

type taskListToolArgs struct {
	ParentTaskID    string   `json:"parent_task_id"`
	AssigneeAgentID string   `json:"assignee_agent_id"`
	States          []string `json:"states"`
}

type taskTransitionToolArgs struct {
	TaskID          string  `json:"task_id"`
	Target          string  `json:"target"`
	ExpectedVersion *uint64 `json:"expected_version"`
	Reason          string  `json:"reason"`
}

type taskVerifyToolArgs struct {
	TaskID          string  `json:"task_id"`
	Passed          bool    `json:"passed"`
	ExpectedVersion *uint64 `json:"expected_version"`
	Reason          string  `json:"reason"`
}

type taskClaimToolArgs struct {
	TaskID          string  `json:"task_id"`
	ExpectedVersion *uint64 `json:"expected_version"`
}

type taskDependencyToolArgs struct {
	TaskID           string `json:"task_id"`
	DependencyTaskID string `json:"dependency_task_id"`
}

type taskReassignToolArgs struct {
	TaskID          string  `json:"task_id"`
	FailedAgentID   string  `json:"failed_agent_id"`
	ExpectedVersion *uint64 `json:"expected_version"`
	Reason          string  `json:"reason"`
}

type taskUpdateToolArgs struct {
	TaskID              string  `json:"task_id"`
	ExpectedVersion     *uint64 `json:"expected_version"`
	Title               *string `json:"title"`
	Description         *string `json:"description"`
	Priority            *int    `json:"priority"`
	RequiredProfile     *string `json:"required_profile"`
	VerificationProfile *string `json:"verification_profile"`
	Reason              string  `json:"reason"`
	IdempotencyKey      string  `json:"idempotency_key"`
}

func (supervisor *Supervisor) taskUpdateTool(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	actor, err := actorFromContext(ctx)
	if err != nil {
		return nil, err
	}
	var args taskUpdateToolArgs
	if err := decodeToolArgs(raw, &args); err != nil {
		return nil, err
	}
	task, err := supervisor.GetTask(ctx, orchestration.TaskID(args.TaskID))
	if err != nil {
		return nil, err
	}
	if task.TeamID != actor.TeamID {
		return nil, errors.New("task_update can only modify tasks in the current Team")
	}
	if args.IdempotencyKey == "" {
		args.IdempotencyKey = orchestration.ToolCallIDFromContext(ctx)
	}
	if args.ExpectedVersion == nil {
		return nil, errors.New("expected_version from task_list is required for plan editing")
	}
	updated, err := supervisor.UpdateTask(ctx, orchestration.TaskUpdate{TaskID: task.ID, Expected: *args.ExpectedVersion, ActorAgentID: actor.AgentID, Title: args.Title, Description: args.Description, Priority: args.Priority, RequiredProfile: args.RequiredProfile, VerificationProfile: args.VerificationProfile, IdempotencyKey: args.IdempotencyKey, CorrelationID: orchestration.ToolCallIDFromContext(ctx), Reason: args.Reason})
	if err != nil {
		return nil, err
	}
	return json.Marshal(updated)
}

func (supervisor *Supervisor) taskCreateTool(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	actor, err := actorFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if err := supervisor.authorizeTaskControl(ctx, actor); err != nil {
		return nil, err
	}
	var args taskCreateToolArgs
	if err := decodeToolArgs(raw, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.Title) == "" {
		return nil, errors.New("title is required")
	}
	if args.IdempotencyKey == "" {
		args.IdempotencyKey = orchestration.ToolCallIDFromContext(ctx)
	}
	spec := orchestration.TaskSpec{
		TeamID: actor.TeamID, ActorAgentID: actor.AgentID, Title: args.Title, Description: args.Description,
		Priority: args.Priority, RequiredProfile: args.RequiredProfile, VerificationProfile: args.VerificationProfile, IdempotencyKey: args.IdempotencyKey,
		CorrelationID: orchestration.ToolCallIDFromContext(ctx), ParentTaskID: orchestration.TaskID(args.ParentTaskID),
	}
	task, err := supervisor.CreateTask(ctx, spec)
	if err != nil {
		return nil, err
	}
	return json.Marshal(task)
}

func (supervisor *Supervisor) taskRefineTool(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	actor, err := actorFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if err := supervisor.authorizeTaskControl(ctx, actor); err != nil {
		return nil, err
	}
	var args taskRefineToolArgs
	if err := decodeToolArgs(raw, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.ParentTaskID) == "" || len(args.Children) == 0 {
		return nil, errors.New("parent_task_id and children are required")
	}
	if args.IdempotencyKey == "" {
		args.IdempotencyKey = orchestration.ToolCallIDFromContext(ctx)
	}
	spec := orchestration.TaskRefinementSpec{TeamID: actor.TeamID, ActorAgentID: actor.AgentID, ParentTaskID: orchestration.TaskID(args.ParentTaskID), Expected: args.ExpectedVersion, IdempotencyKey: args.IdempotencyKey, CorrelationID: orchestration.ToolCallIDFromContext(ctx)}
	for _, child := range args.Children {
		spec.Children = append(spec.Children, orchestration.TaskRefinementChild{Key: child.Key, Title: child.Title, Description: child.Description, Priority: child.Priority, RequiredProfile: child.RequiredProfile, VerificationProfile: child.VerificationProfile})
	}
	for _, dependency := range args.Dependencies {
		spec.Dependencies = append(spec.Dependencies, orchestration.TaskRefinementDependency{ChildKey: dependency.ChildKey, DependencyChildKey: dependency.DependencyChildKey, DependencyTaskID: orchestration.TaskID(dependency.DependencyTaskID)})
	}
	result, err := supervisor.RefineTasks(ctx, spec)
	if err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

func (supervisor *Supervisor) taskListTool(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	actor, err := actorFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if err := supervisor.authorizeTaskControl(ctx, actor); err != nil {
		return nil, err
	}
	var args taskListToolArgs
	if err := decodeToolArgs(raw, &args); err != nil {
		return nil, err
	}
	query := orchestration.TaskQuery{
		TeamID: actor.TeamID, ParentTaskID: orchestration.TaskID(args.ParentTaskID), AssigneeAgentID: orchestration.AgentID(args.AssigneeAgentID),
	}
	for _, state := range args.States {
		query.States = append(query.States, orchestration.TaskState(state))
	}
	tasks, err := supervisor.ListTasks(ctx, query)
	if err != nil {
		return nil, err
	}
	return json.Marshal(tasks)
}

func (supervisor *Supervisor) taskTransitionTool(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	actor, err := actorFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if err := supervisor.authorizeTaskControl(ctx, actor); err != nil {
		return nil, err
	}
	var args taskTransitionToolArgs
	if err := decodeToolArgs(raw, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.TaskID) == "" || strings.TrimSpace(args.Target) == "" {
		return nil, errors.New("task_id and target are required")
	}
	task, err := supervisor.GetTask(ctx, orchestration.TaskID(args.TaskID))
	if err != nil {
		return nil, err
	}
	if task.TeamID != actor.TeamID {
		return nil, errors.New("task_transition can only modify tasks in the current Team")
	}
	if err := supervisor.authorizeTaskMutation(ctx, actor, task); err != nil {
		return nil, err
	}
	transition := orchestration.TaskTransition{
		TaskID: task.ID, Expected: expectedTaskVersion(args.ExpectedVersion), Target: orchestration.TaskState(args.Target),
		ActorAgentID: actor.AgentID, CorrelationID: orchestration.ToolCallIDFromContext(ctx), Reason: args.Reason,
	}
	if transition.Target == orchestration.TaskClaimed || transition.Target == orchestration.TaskInProgress {
		transition.AssigneeAgentID = actor.AgentID
	}
	if transition.Target == orchestration.TaskCompleted {
		if err := supervisor.authorizeTaskReviewer(ctx, actor, task); err != nil {
			return nil, err
		}
	}
	updated, err := supervisor.TransitionTask(ctx, transition)
	if err != nil {
		return nil, err
	}
	return json.Marshal(updated)
}

func (supervisor *Supervisor) taskVerifyTool(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	actor, err := actorFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if err := supervisor.authorizeTaskControl(ctx, actor); err != nil {
		return nil, err
	}
	var args taskVerifyToolArgs
	if err := decodeToolArgs(raw, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.TaskID) == "" {
		return nil, errors.New("task_id is required")
	}
	task, err := supervisor.GetTask(ctx, orchestration.TaskID(args.TaskID))
	if err != nil {
		return nil, err
	}
	if task.TeamID != actor.TeamID {
		return nil, errors.New("task_verify can only modify tasks in the current Team")
	}
	if task.State != orchestration.TaskVerification {
		return nil, fmt.Errorf("%w: task %s is in state %s", orchestration.ErrInvalidEvent, task.ID, task.State)
	}
	if err := supervisor.authorizeTaskReviewer(ctx, actor, task); err != nil {
		return nil, err
	}
	if err := supervisor.authorizeTaskMutation(ctx, actor, task); err != nil {
		return nil, err
	}
	target := orchestration.TaskReady
	if args.Passed {
		target, err = supervisor.verifiedTarget(ctx, task)
		if err != nil {
			return nil, err
		}
	}
	updated, err := supervisor.TransitionTask(ctx, orchestration.TaskTransition{
		TaskID: task.ID, Expected: expectedTaskVersion(args.ExpectedVersion), Target: target,
		ActorAgentID: actor.AgentID, CorrelationID: orchestration.ToolCallIDFromContext(ctx), Reason: args.Reason,
	})
	if err != nil {
		return nil, err
	}
	// A passed candidate becomes ready to merge; a failed one is rejected.
	supervisor.scheduleMerges(task.TeamID)
	return json.Marshal(updated)
}

// verifiedTarget is the state of a task whose review passed. A reviewed
// snapshot candidate or worktree patch still has to reach main before the
// task completes.
func (supervisor *Supervisor) verifiedTarget(ctx context.Context, task orchestration.TaskSnapshot) (orchestration.TaskState, error) {
	if supervisor.workspace == nil || supervisor.workspace.Repository == nil {
		return orchestration.TaskCompleted, nil
	}
	merges, err := supervisor.workspace.Repository.ListMerges(ctx, workspacecoord.MergeQuery{
		TeamID: task.TeamID, TaskID: task.ID, States: openMergeStates(),
	})
	if err != nil {
		return "", fmt.Errorf("list open merges of task %s: %w", task.ID, err)
	}
	if len(merges) > 0 {
		return orchestration.TaskAwaitingMerge, nil
	}
	return orchestration.TaskCompleted, nil
}

func (supervisor *Supervisor) taskClaimTool(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	actor, err := actorFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if err := supervisor.authorizeTaskControl(ctx, actor); err != nil {
		return nil, err
	}
	var args taskClaimToolArgs
	if err := decodeToolArgs(raw, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.TaskID) == "" {
		return nil, errors.New("task_id is required")
	}
	task, err := supervisor.GetTask(ctx, orchestration.TaskID(args.TaskID))
	if err != nil {
		return nil, err
	}
	if task.TeamID != actor.TeamID {
		return nil, errors.New("task_claim can only claim tasks in the current Team")
	}
	claimed, err := supervisor.ClaimTask(ctx, task.ID, actor.AgentID, expectedTaskVersion(args.ExpectedVersion))
	if err != nil {
		return nil, err
	}
	return json.Marshal(claimed)
}

func (supervisor *Supervisor) taskReleaseTool(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	actor, err := actorFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if err := supervisor.authorizeTaskControl(ctx, actor); err != nil {
		return nil, err
	}
	var args taskClaimToolArgs
	if err := decodeToolArgs(raw, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.TaskID) == "" {
		return nil, errors.New("task_id is required")
	}
	task, err := supervisor.GetTask(ctx, orchestration.TaskID(args.TaskID))
	if err != nil {
		return nil, err
	}
	if task.TeamID != actor.TeamID {
		return nil, errors.New("task_release can only release tasks in the current Team")
	}
	if task.AssigneeAgentID != actor.AgentID {
		return nil, errors.New("task_release can only release tasks assigned to the current Agent")
	}
	released, err := supervisor.ReleaseTask(ctx, task.ID, actor.AgentID, expectedTaskVersion(args.ExpectedVersion))
	if err != nil {
		return nil, err
	}
	return json.Marshal(released)
}

func (supervisor *Supervisor) taskReassignTool(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	actor, err := actorFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if err := supervisor.authorizeTaskControl(ctx, actor); err != nil {
		return nil, err
	}
	var args taskReassignToolArgs
	if err := decodeToolArgs(raw, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.TaskID) == "" {
		return nil, errors.New("task_id is required")
	}
	task, err := supervisor.GetTask(ctx, orchestration.TaskID(args.TaskID))
	if err != nil {
		return nil, err
	}
	if task.TeamID != actor.TeamID {
		return nil, errors.New("task_reassign can only modify tasks in the current Team")
	}
	if err := supervisor.authorizeTaskMutation(ctx, actor, task); err != nil {
		return nil, err
	}
	failedAgent := orchestration.AgentID(args.FailedAgentID)
	if failedAgent == "" {
		failedAgent = task.AssigneeAgentID
	}
	if failedAgent == "" {
		return nil, errors.New("failed_agent_id is required when the task has no assignee")
	}
	expected := task.Version
	if args.ExpectedVersion != nil {
		expected = *args.ExpectedVersion
	}
	updated, err := supervisor.ReassignTask(ctx, task.ID, failedAgent, expected, args.Reason)
	if err != nil {
		return nil, err
	}
	return json.Marshal(updated)
}

func (supervisor *Supervisor) taskAddDependencyTool(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	return supervisor.taskDependencyTool(ctx, raw, true)
}

func (supervisor *Supervisor) taskRemoveDependencyTool(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	return supervisor.taskDependencyTool(ctx, raw, false)
}

func (supervisor *Supervisor) taskDependencyTool(ctx context.Context, raw json.RawMessage, add bool) (json.RawMessage, error) {
	actor, err := actorFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if err := supervisor.authorizeTaskControl(ctx, actor); err != nil {
		return nil, err
	}
	var args taskDependencyToolArgs
	if err := decodeToolArgs(raw, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.TaskID) == "" || strings.TrimSpace(args.DependencyTaskID) == "" {
		return nil, errors.New("task_id and dependency_task_id are required")
	}
	taskID, dependencyID := orchestration.TaskID(args.TaskID), orchestration.TaskID(args.DependencyTaskID)
	if task, getErr := supervisor.GetTask(ctx, taskID); getErr != nil {
		return nil, getErr
	} else if task.TeamID != actor.TeamID {
		return nil, errors.New("task dependency can only modify tasks in the current Team")
	} else if err := supervisor.authorizeTaskMutation(ctx, actor, task); err != nil {
		return nil, err
	}
	if add {
		err = supervisor.AddTaskDependency(ctx, taskID, dependencyID, actor.AgentID, orchestration.ToolCallIDFromContext(ctx))
	} else {
		err = supervisor.RemoveTaskDependency(ctx, taskID, dependencyID, actor.AgentID, orchestration.ToolCallIDFromContext(ctx))
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"task_id": taskID, "dependency_task_id": dependencyID,
		"action": map[bool]string{true: "added", false: "removed"}[add],
	})
}

func expectedTaskVersion(version *uint64) uint64 {
	if version == nil {
		return orchestration.AnySequence
	}
	return *version
}

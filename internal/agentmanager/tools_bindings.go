package agentmanager

import (
	"encoding/json"

	appTool "github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

// Bindings exposes the Supervisor command surface as ordinary application
// tools. The executor only accepts JSON; actor identity comes from the trusted
// runtime context and cannot be supplied by the model.
func Bindings(supervisor *Supervisor) []appTool.Binding {
	if supervisor == nil {
		return nil
	}
	bindings := []appTool.Binding{
		binding("agent_spawn", "Create one durable child Agent and start it asynchronously.", spawnSchema, appTool.RiskWrite, appTool.InvokeFunc(supervisor.spawnTool)),
		binding("agent_continue", "Run another stage in an existing durable child Agent session.", continueSchema, appTool.RiskWrite, appTool.InvokeFunc(supervisor.continueTool)),
		binding("agent_resume", "Resume a child Agent paused by a recoverable external failure.", resumeSchema, appTool.RiskWrite, appTool.InvokeFunc(supervisor.resumeTool)),
		binding("agent_list", "List Agents in the current Team.", listSchema, appTool.RiskRead, appTool.InvokeFunc(supervisor.listTool)),
		binding("agent_wait", "Wait until one or more child Agents reach a terminal state.", waitSchema, appTool.RiskRead, appTool.InvokeFunc(supervisor.waitTool)),
		binding("agent_interrupt", "Request cancellation of a child Agent.", interruptSchema, appTool.RiskWrite, appTool.InvokeFunc(supervisor.interruptTool)),
		binding("agent_message", "Send a durable message to a directly related Agent.", messageSchema, appTool.RiskWrite, appTool.InvokeFunc(supervisor.messageTool)),
	}
	if supervisor.tasks != nil {
		bindings = append(bindings,
			binding("task_create", "Create a durable task in the current Team.", taskCreateSchema, appTool.RiskWrite, appTool.InvokeFunc(supervisor.taskCreateTool)),
			binding("task_refine", "Atomically split a live task into bounded child tasks and dependencies.", taskRefineSchema, appTool.RiskWrite, appTool.InvokeFunc(supervisor.taskRefineTool)),
			binding("task_update", "Edit an unstarted draft, blocked or ready task using its current version.", taskUpdateSchema, appTool.RiskWrite, appTool.InvokeFunc(supervisor.taskUpdateTool)),
			binding("task_list", "List durable tasks in the current Team.", taskListSchema, appTool.RiskRead, appTool.InvokeFunc(supervisor.taskListTool)),
			binding("task_transition", "Advance a task through its validated lifecycle.", taskTransitionSchema, appTool.RiskWrite, appTool.InvokeFunc(supervisor.taskTransitionTool)),
			binding("task_verify", "Accept or reject a task from a separate reviewer Agent.", taskVerifySchema, appTool.RiskWrite, appTool.InvokeFunc(supervisor.taskVerifyTool)),
			binding("task_claim", "Claim a ready task for the current Agent.", taskClaimSchema, appTool.RiskWrite, appTool.InvokeFunc(supervisor.taskClaimTool)),
			binding("task_release", "Release a claimed task back to the ready queue.", taskReleaseSchema, appTool.RiskWrite, appTool.InvokeFunc(supervisor.taskReleaseTool)),
			binding("task_reassign", "Reassign a trusted failed attempt only after host safety checks; writes and unknown effects are rejected.", taskReassignSchema, appTool.RiskWrite, appTool.InvokeFunc(supervisor.taskReassignTool)),
			binding("task_add_dependency", "Add a dependency edge to a task.", taskDependencySchema, appTool.RiskWrite, appTool.InvokeFunc(supervisor.taskAddDependencyTool)),
			binding("task_remove_dependency", "Remove a dependency edge from a task.", taskDependencySchema, appTool.RiskWrite, appTool.InvokeFunc(supervisor.taskRemoveDependencyTool)),
		)
	}
	if supervisor.workspace != nil {
		bindings = append(bindings,
			binding("merge_list", "List workspace merge requests in the current Team.", mergeListSchema, appTool.RiskRead, appTool.InvokeFunc(supervisor.mergeListTool)),
			binding("merge_requeue", "Requeue a conflicted merge after its resolution task completed.", mergeRequeueSchema, appTool.RiskWrite, appTool.InvokeFunc(supervisor.mergeRequeueTool)),
			binding("merge_run", "Apply queued workspace merges for the current Team.", mergeRunSchema, appTool.RiskWrite, appTool.InvokeFunc(supervisor.mergeRunTool)),
		)
	}
	return bindings
}

const (
	spawnSchema          = `{"type":"object","properties":{"prompt":{"type":"string","description":"The task for the child Agent"},"profile":{"type":"string","description":"Optional Agent profile name, such as worker or reviewer; omit to inherit the parent's profile"},"task_id":{"type":"string","description":"Optional existing task ID returned by task_create; omit for ad-hoc child work"},"context_policy":{"type":"string","enum":["none","summary","recent","full"]},"workspace":{"type":"string","enum":["auto","readonly"],"description":"Workspace access for the child: auto keeps its profile's access, readonly forbids workspace writes and shell commands"},"idempotency_key":{"type":"string"}},"required":["prompt"]}`
	continueSchema       = `{"type":"object","properties":{"agent_id":{"type":"string"},"prompt":{"type":"string"},"idempotency_key":{"type":"string"}},"required":["agent_id","prompt"]}`
	resumeSchema         = `{"type":"object","properties":{"agent_id":{"type":"string"}},"required":["agent_id"]}`
	listSchema           = `{"type":"object","properties":{"parent_agent_id":{"type":"string"},"states":{"type":"array","items":{"type":"string"}}}}`
	waitSchema           = `{"type":"object","properties":{"agent_ids":{"type":"array","items":{"type":"string"},"minItems":1},"any":{"type":"boolean"},"timeout_ms":{"type":"integer","minimum":1}},"required":["agent_ids"]}`
	interruptSchema      = `{"type":"object","properties":{"agent_id":{"type":"string"}},"required":["agent_id"]}`
	messageSchema        = `{"type":"object","properties":{"recipient_agent_id":{"type":"string"},"type":{"type":"string","enum":["directive","question","answer","context","status","result","control"]},"body":{},"idempotency_key":{"type":"string"}},"required":["recipient_agent_id","type","body"]}`
	taskCreateSchema     = `{"type":"object","properties":{"title":{"type":"string"},"description":{"type":"string"},"parent_task_id":{"type":"string"},"priority":{"type":"integer"},"required_profile":{"type":"string"},"verification_profile":{"type":"string"},"idempotency_key":{"type":"string"}},"required":["title"]}`
	taskRefineSchema     = `{"type":"object","properties":{"parent_task_id":{"type":"string"},"expected_version":{"type":"integer","minimum":1},"idempotency_key":{"type":"string"},"children":{"type":"array","minItems":1,"maxItems":64,"items":{"type":"object","properties":{"key":{"type":"string"},"title":{"type":"string"},"description":{"type":"string"},"priority":{"type":"integer"},"required_profile":{"type":"string"},"verification_profile":{"type":"string"}},"required":["key","title"]}},"dependencies":{"type":"array","maxItems":256,"items":{"type":"object","properties":{"child_key":{"type":"string"},"dependency_child_key":{"type":"string"},"dependency_task_id":{"type":"string"}},"required":["child_key"]}}},"required":["parent_task_id","expected_version","children"]}`
	taskUpdateSchema     = `{"type":"object","properties":{"task_id":{"type":"string"},"expected_version":{"type":"integer","minimum":1},"title":{"type":"string"},"description":{"type":"string"},"priority":{"type":"integer"},"required_profile":{"type":"string"},"verification_profile":{"type":"string"},"reason":{"type":"string"},"idempotency_key":{"type":"string"}},"required":["task_id","expected_version"]}`
	taskListSchema       = `{"type":"object","properties":{"parent_task_id":{"type":"string"},"assignee_agent_id":{"type":"string"},"states":{"type":"array","items":{"type":"string"}}}}`
	taskTransitionSchema = `{"type":"object","properties":{"task_id":{"type":"string"},"target":{"type":"string","enum":["draft","blocked","ready","claimed","in_progress","verification","completed","failed","canceled"]},"expected_version":{"type":"integer","minimum":1},"reason":{"type":"string"}},"required":["task_id","target"]}`
	taskVerifySchema     = `{"type":"object","properties":{"task_id":{"type":"string"},"passed":{"type":"boolean"},"expected_version":{"type":"integer","minimum":1},"reason":{"type":"string"}},"required":["task_id","passed"]}`
	taskClaimSchema      = `{"type":"object","properties":{"task_id":{"type":"string"},"expected_version":{"type":"integer","minimum":1}},"required":["task_id"]}`
	taskReleaseSchema    = taskClaimSchema
	taskReassignSchema   = `{"type":"object","properties":{"task_id":{"type":"string"},"failed_agent_id":{"type":"string"},"expected_version":{"type":"integer","minimum":1},"reason":{"type":"string"}},"required":["task_id"]}`
	taskDependencySchema = `{"type":"object","properties":{"task_id":{"type":"string"},"dependency_task_id":{"type":"string"}},"required":["task_id","dependency_task_id"]}`
	mergeListSchema      = `{"type":"object","properties":{"states":{"type":"array","items":{"type":"string"}}}}`
	mergeRequeueSchema   = `{"type":"object","properties":{"merge_id":{"type":"string"},"resolution_task_id":{"type":"string"}},"required":["merge_id","resolution_task_id"]}`
	mergeRunSchema       = `{"type":"object","properties":{"limit":{"type":"integer","minimum":1}}}`
)

func binding(name, description, schema string, risk appTool.RiskClass, executor appTool.Executor) appTool.Binding {
	return appTool.Binding{Descriptor: appTool.Descriptor{
		Name: name, Description: description, InputSchema: json.RawMessage(schema), Revision: "builtin-v1", RiskClass: risk,
		Capabilities: []string{"agent.control"},
	}, Executor: executor}
}
